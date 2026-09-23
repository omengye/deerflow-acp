from collections.abc import AsyncIterator, Iterable
import asyncio
from concurrent.futures import Future
import json
import threading
from types import SimpleNamespace
from typing import Any, override
import unittest
from unittest.mock import AsyncMock, Mock, patch

from app import dependencies
from app.channels.feishu import (
    FeishuChannel,
    _IncomingResource,
    _filename_for_incoming_resource,
    _incoming_resources,
    _make_proposal_card,
    _parse_proposal_action,
)
from deerflow.client import StreamEvent
from deerflow.runtime import MemoryStreamBridge
from deerflow.skills.evolution.models import ProposalTrigger, SkillProposal


class _FakeClient:
    def __init__(
        self,
        events: Iterable[StreamEvent],
        *,
        yield_between_events: bool = False,
        error_after_events: Exception | None = None,
    ) -> None:
        self.events: list[StreamEvent] = list(events)
        self.yield_between_events = yield_between_events
        self.error_after_events = error_after_events
        self.last_message: str | None = None
        self.last_thread_id: str | None = None

    async def astream(self, message: str, *, thread_id: str) -> AsyncIterator[StreamEvent]:
        self.last_message = message
        self.last_thread_id = thread_id
        for event in self.events:
            yield event
            if self.yield_between_events:
                await asyncio.sleep(0)
        if self.error_after_events is not None:
            raise self.error_after_events


class _FakeManager:
    def __init__(self, client: _FakeClient, stream_bridge: MemoryStreamBridge | None = None) -> None:
        self.client: _FakeClient = client
        self.stream_bridge = stream_bridge

    async def get_async_client(self) -> _FakeClient:
        return self.client


class _RecordingFeishuChannel(FeishuChannel):
    def __init__(self) -> None:
        super().__init__(app_id="app", app_secret="secret")
        self.sent_cards: list[tuple[str, str, str]] = []
        self.patched_cards: list[tuple[str, str]] = []
        self.reactions: list[tuple[str, str]] = []
        self.sent_artifacts: list[tuple[str, str, tuple[str, ...]]] = []
        self.saved_resources: list[tuple[str, tuple[str, ...]]] = []
        self.incoming_uploads: list[dict[str, Any]] = []
        self.resource_save_error: Exception | None = None
        self.operation_log: list[str] = []
        self._next_card_number: int = 1
        self.release_send_card: asyncio.Event | None = None
        self.release_patch_card: asyncio.Event | None = None
        self.stream_started: asyncio.Event | None = None

    async def handle_message_for_test(
        self,
        client: _FakeClient,
        *,
        text: str = "prompt",
        resources: list[_IncomingResource] | None = None,
    ) -> None:
        original_get_client_manager = dependencies.get_client_manager
        dependencies.get_client_manager = lambda: _FakeManager(client)
        try:
            await self._handle_message(
                message_id="user-message",
                chat_id="chat-1",
                text=text,
                thread_id="thread-1",
                resources=resources,
            )
        finally:
            dependencies.get_client_manager = original_get_client_manager

    @override
    async def _send_card(self, chat_id: str, content: str) -> str:
        if self.release_send_card is not None:
            _ = await self.release_send_card.wait()
        card_id = f"card-{self._next_card_number}"
        self._next_card_number += 1
        self.sent_cards.append((chat_id, content, card_id))
        self.operation_log.append(f"send:{card_id}:{content}")
        return card_id

    @override
    async def _patch_card(self, card_id: str, content: str) -> None:
        if self.release_patch_card is not None:
            _ = await self.release_patch_card.wait()
        self.patched_cards.append((card_id, content))
        _ = self.operation_log.append(f"patch:{card_id}:{content}")

    @override
    async def _add_reaction(self, message_id: str, emoji_type: str) -> None:
        self.reactions.append((message_id, emoji_type))
        _ = self.operation_log.append(f"reaction:{emoji_type}")

    @override
    async def _send_artifacts(self, chat_id: str, thread_id: str, artifacts: list[str]) -> int:
        if not artifacts:
            return 0
        self.sent_artifacts.append((chat_id, thread_id, tuple(artifacts)))
        self._sent_artifacts_by_thread.setdefault(thread_id, set()).update(artifacts)
        return len(artifacts)

    @override
    async def _save_incoming_resources(self, thread_id: str, resources: list[_IncomingResource]) -> list[dict[str, Any]]:
        if self.resource_save_error is not None:
            raise self.resource_save_error
        self.saved_resources.append((thread_id, tuple(resource.resource_key for resource in resources)))
        return self.incoming_uploads

    @override
    async def _send_pending_proposal_cards(self, thread_id: str, chat_id: str, *, force: bool) -> int:
        return 0

    @override
    async def _stream_to_cards(self, text: str, thread_id: str, chat_id: str, initial_card_id: str) -> str:
        if self.stream_started is not None:
            _ = self.stream_started.set()
        return await super()._stream_to_cards(text, thread_id, chat_id, initial_card_id)

    async def stream_to_cards(self, client: _FakeClient) -> str:
        original_get_client_manager = dependencies.get_client_manager
        dependencies.get_client_manager = lambda: _FakeManager(client)
        try:
            return await self._stream_to_cards("prompt", "thread-1", "chat-1", "initial-card")
        finally:
            dependencies.get_client_manager = original_get_client_manager

    async def render_run_to_chat_for_test(self, stream_bridge: MemoryStreamBridge) -> str:
        original_get_client_manager = dependencies.get_client_manager
        dependencies.get_client_manager = lambda: _FakeManager(_FakeClient([]), stream_bridge)
        try:
            return await self.render_run_to_chat(run_id="run-1", thread_id="thread-1", chat_id="chat-1")
        finally:
            dependencies.get_client_manager = original_get_client_manager


class _ProposalRecordingFeishuChannel(FeishuChannel):
    def __init__(self) -> None:
        super().__init__(app_id="app", app_secret="secret")
        self.sent_raw_cards: list[tuple[str, str]] = []
        self.patched_raw_cards: list[tuple[str, str]] = []
        self.sent_cards: list[tuple[str, str]] = []
        self.reactions: list[tuple[str, str]] = []
        self.stream_calls = 0

    @override
    async def _send_raw_card(self, chat_id: str, card: str) -> str:
        self.sent_raw_cards.append((chat_id, card))
        return f"proposal-card-{len(self.sent_raw_cards)}"

    @override
    async def _patch_raw_card(self, card_id: str, card: str) -> None:
        self.patched_raw_cards.append((card_id, card))

    @override
    async def _send_card(self, chat_id: str, content: str) -> str:
        self.sent_cards.append((chat_id, content))
        return f"text-card-{len(self.sent_cards)}"

    @override
    async def _add_reaction(self, message_id: str, emoji_type: str) -> None:
        self.reactions.append((message_id, emoji_type))

    @override
    async def _stream_to_cards(self, text: str, thread_id: str, chat_id: str, initial_card_id: str) -> str:
        self.stream_calls += 1
        return "done"


def _proposal(*, status: str = "pending_review", proposal_id: str = "p_test") -> SkillProposal:
    return SkillProposal(
        id=proposal_id,
        status=status,
        action="edit",
        skill_name="sample-skill",
        reason="Improve the workflow",
        trigger=ProposalTrigger(thread_id="thread-1"),
        risk="medium",
        created_at="2026-07-23T00:00:00+00:00",
        updated_at="2026-07-23T00:00:00+00:00",
    )


class FeishuChannelTests(unittest.IsolatedAsyncioTestCase):
    async def test_incoming_resource_download_is_size_bounded_and_closes_stream(self) -> None:
        channel = FeishuChannel(app_id="app", app_secret="secret")
        read_sizes: list[int] = []

        class Stream:
            closed = False

            def read(self, size: int) -> bytes:
                read_sizes.append(size)
                return b"12345"

            def close(self) -> None:
                self.closed = True

        stream = Stream()
        response = SimpleNamespace(
            success=lambda: True,
            file=stream,
            file_name="large.bin",
        )
        request_type = Mock()
        builder = request_type.builder.return_value
        builder.message_id.return_value = builder
        builder.file_key.return_value = builder
        builder.type.return_value = builder
        get_resource = Mock(return_value=response)
        channel._lark_client = SimpleNamespace(
            im=SimpleNamespace(
                v1=SimpleNamespace(
                    message_resource=SimpleNamespace(get=get_resource),
                )
            )
        )
        resource = _IncomingResource(
            resource_type="file",
            resource_key="file-key",
            filename="large.bin",
            message_id="message-1",
        )

        with (
            patch("app.channels.feishu._MAX_FEISHU_FILE_BYTES", 4),
            patch.dict(
                "sys.modules",
                {"lark_oapi.api.im.v1": SimpleNamespace(GetMessageResourceRequest=request_type)},
            ),
        ):
            with self.assertRaisesRegex(ValueError, "inbound file limit"):
                await channel._download_message_resource(resource)

        request_type.builder.assert_called_once_with()
        builder.message_id.assert_called_once_with("message-1")
        builder.file_key.assert_called_once_with("file-key")
        builder.type.assert_called_once_with("file")
        builder.build.assert_called_once_with()
        get_resource.assert_called_once_with(builder.build.return_value)
        self.assertEqual(read_sizes, [5])
        self.assertTrue(stream.closed)

    async def test_attachment_upload_helpers_run_off_event_loop(self) -> None:
        channel = FeishuChannel(app_id="app", app_secret="secret")
        loop_thread = threading.get_ident()
        worker_threads: list[int] = []

        def image_upload(_path):
            worker_threads.append(threading.get_ident())
            return SimpleNamespace(success=lambda: True, data=SimpleNamespace(image_key="image-key"))

        def file_upload(_path):
            worker_threads.append(threading.get_ident())
            return SimpleNamespace(success=lambda: True, data=SimpleNamespace(file_key="file-key"))

        with (
            patch.object(channel, "_upload_image_sync", side_effect=image_upload),
            patch.object(channel, "_upload_file_sync", side_effect=file_upload),
        ):
            self.assertEqual(await channel._upload_image(SimpleNamespace()), "image-key")
            self.assertEqual(await channel._upload_file(SimpleNamespace()), "file-key")

        self.assertEqual(len(worker_threads), 2)
        self.assertTrue(all(thread_id != loop_thread for thread_id in worker_threads))

    async def test_render_run_to_chat_consumes_stream_bridge_events(self) -> None:
        stream_bridge = MemoryStreamBridge(queue_maxsize=32)
        await stream_bridge.publish("run-1", "metadata", {"run_id": "run-1", "thread_id": "thread-1"})
        await stream_bridge.publish("run-1", "messages-tuple", {"type": "ai", "content": "hello", "id": "ai-1", "is_delta": True})
        await stream_bridge.publish_end("run-1")

        channel = _RecordingFeishuChannel()
        final_text = await channel.render_run_to_chat_for_test(stream_bridge)

        self.assertEqual(final_text, "hello")
        self.assertEqual(channel.sent_cards, [("chat-1", "Scheduled task started...", "card-1")])
        self.assertEqual(channel.patched_cards, [("card-1", "hello")])

    async def test_stream_updates_same_card_for_same_ai_message_id(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "hel", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "lo", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        final_text = await channel.stream_to_cards(fake_client)

        self.assertEqual(final_text, "hello")
        self.assertEqual(channel.sent_cards, [])
        self.assertEqual(channel.patched_cards, [("initial-card", "hello")])
        self.assertEqual(channel.patched_cards[-1], ("initial-card", "hello"))
        self.assertTrue(all(card_id == "initial-card" for card_id, _content in channel.patched_cards))

    async def test_stream_creates_new_card_for_new_ai_message_id(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "first", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "second", "id": "ai-2", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        final_text = await channel.stream_to_cards(fake_client)

        self.assertEqual(final_text, "second")
        self.assertEqual(channel.sent_cards, [("chat-1", "second", "card-1")])
        self.assertIn(("initial-card", "first"), channel.patched_cards)
        self.assertIn(("card-1", "second"), channel.patched_cards)

    async def test_stream_ignores_non_delta_replayed_ai_text_for_card_creation(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "old", "id": "old-ai"}),
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "new", "id": "new-ai", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        final_text = await channel.stream_to_cards(fake_client)

        self.assertEqual(final_text, "new")
        self.assertEqual(channel.sent_cards, [])
        self.assertEqual(channel.patched_cards[-1], ("initial-card", "new"))

    async def test_stream_waits_for_slow_partial_patch_before_final_patch(self) -> None:
        partial = "this is a slow partial chunk"
        final = f"{partial} final"
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": partial, "id": "ai-1", "is_delta": True}),
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": " final", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ],
            yield_between_events=True,
        )
        channel = _RecordingFeishuChannel()
        channel.release_patch_card = asyncio.Event()
        stream_task = asyncio.create_task(channel.stream_to_cards(fake_client))
        await asyncio.sleep(0)

        self.assertFalse(stream_task.done())
        self.assertEqual(channel.patched_cards, [])
        _ = channel.release_patch_card.set()

        final_text = await stream_task

        self.assertEqual(final_text, final)
        self.assertEqual(channel.patched_cards[-2:], [("initial-card", partial), ("initial-card", final)])

    async def test_stream_cancels_background_patches_when_agent_stream_fails(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(
                    type="messages-tuple",
                    data={
                        "type": "ai",
                        "content": "this partial chunk should be cancelled",
                        "id": "ai-1",
                        "is_delta": True,
                    },
                ),
            ],
            yield_between_events=True,
            error_after_events=RuntimeError("stream failed"),
        )
        channel = _RecordingFeishuChannel()
        channel.release_patch_card = asyncio.Event()

        with self.assertRaises(RuntimeError):
            await channel.stream_to_cards(fake_client)

        self.assertEqual(channel.patched_cards, [])
        channel.release_patch_card.set()

    async def test_stream_waits_for_slow_new_card_creation_before_final_patch(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "first", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "second", "id": "ai-2", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        channel.release_send_card = asyncio.Event()
        stream_task = asyncio.create_task(channel.stream_to_cards(fake_client))
        await asyncio.sleep(0)

        self.assertFalse(stream_task.done())
        self.assertEqual(channel.sent_cards, [])
        _ = channel.release_send_card.set()

        final_text = await stream_task

        self.assertEqual(final_text, "second")
        self.assertEqual(channel.sent_cards, [("chat-1", "second", "card-1")])
        self.assertIn(("card-1", "second"), channel.patched_cards)

    async def test_stream_leaves_initial_card_unchanged_when_only_non_delta_ai_text_arrives(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "snapshot", "id": "old-ai"}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        final_text = await channel.stream_to_cards(fake_client)

        self.assertEqual(final_text, "")
        self.assertEqual(channel.sent_cards, [])
        self.assertEqual(channel.patched_cards, [])

    async def test_stream_sends_unique_artifacts_from_values_events(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="values", data={"artifacts": ["/mnt/user-data/outputs/image.png"]}),
                StreamEvent(type="values", data={"artifacts": ["/mnt/user-data/outputs/image.png", "/mnt/user-data/outputs/report.pdf"]}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        final_text = await channel.stream_to_cards(fake_client)

        self.assertEqual(final_text, "")
        self.assertEqual(
            channel.sent_artifacts,
            [("chat-1", "thread-1", ("/mnt/user-data/outputs/image.png", "/mnt/user-data/outputs/report.pdf"))],
        )
        self.assertEqual(channel.patched_cards, [("initial-card", "✅ 已发送 2 个生成文件。")])

    async def test_stream_does_not_resend_artifacts_already_sent_for_thread(self) -> None:
        channel = _RecordingFeishuChannel()
        first_client = _FakeClient(
            [
                StreamEvent(type="values", data={"artifacts": ["/mnt/user-data/outputs/image.png"]}),
                StreamEvent(type="end", data={}),
            ]
        )
        second_client = _FakeClient(
            [
                StreamEvent(type="values", data={"artifacts": ["/mnt/user-data/outputs/image.png"]}),
                StreamEvent(type="end", data={}),
            ]
        )

        _ = await channel.stream_to_cards(first_client)
        _ = await channel.stream_to_cards(second_client)

        self.assertEqual(
            channel.sent_artifacts,
            [("chat-1", "thread-1", ("/mnt/user-data/outputs/image.png",))],
        )

    async def test_incoming_resources_parse_image_and_file_content(self) -> None:
        image_resources = _incoming_resources("image", "message-1", {"image_key": "img-key"})
        file_resources = _incoming_resources("file", "message-2", {"file_key": "file-key", "file_name": "report.pdf"})

        self.assertEqual(
            image_resources,
            [_IncomingResource(resource_type="image", resource_key="img-key", filename="img-key.png", message_id="message-1")],
        )
        self.assertEqual(
            file_resources,
            [_IncomingResource(resource_type="file", resource_key="file-key", filename="report.pdf", message_id="message-2")],
        )

    async def test_incoming_image_filename_matches_downloaded_bytes(self) -> None:
        resource = _IncomingResource(
            resource_type="image",
            resource_key="img-key",
            filename="img-key.png",
            message_id="message-1",
        )

        filename = _filename_for_incoming_resource(resource, "", b"\xff\xd8\xff\x00")

        self.assertEqual(filename, "img-key.jpg")

    async def test_handle_message_passes_downloaded_resource_paths_to_agent(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "done", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        channel.incoming_uploads = [
            {
                "filename": "photo.png",
                "virtual_path": "/mnt/user-data/uploads/photo.png",
            }
        ]
        resources = [
            _IncomingResource(
                resource_type="image",
                resource_key="img-key",
                filename="photo.png",
                message_id="user-message",
            )
        ]

        await channel.handle_message_for_test(fake_client, text="分析这张图", resources=resources)

        self.assertEqual(channel.saved_resources, [("thread-1", ("img-key",))])
        self.assertEqual(fake_client.last_thread_id, "thread-1")
        self.assertIsNotNone(fake_client.last_message)
        self.assertIn("分析这张图", fake_client.last_message or "")
        self.assertIn("/mnt/user-data/uploads/photo.png", fake_client.last_message or "")
        self.assertIn("view_image", fake_client.last_message or "")

    async def test_handle_message_passes_file_markdown_path_to_agent(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "done", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        channel.incoming_uploads = [
            {
                "filename": "report.pdf",
                "virtual_path": "/mnt/user-data/uploads/report.pdf",
                "markdown_virtual_path": "/mnt/user-data/uploads/report.md",
            }
        ]
        resources = [
            _IncomingResource(
                resource_type="file",
                resource_key="file-key",
                filename="report.pdf",
                message_id="user-message",
            )
        ]

        await channel.handle_message_for_test(fake_client, text="", resources=resources)

        self.assertEqual(channel.saved_resources, [("thread-1", ("file-key",))])
        self.assertIsNotNone(fake_client.last_message)
        self.assertIn("请处理用户通过飞书上传的文件。", fake_client.last_message or "")
        self.assertIn("/mnt/user-data/uploads/report.pdf", fake_client.last_message or "")
        self.assertIn("/mnt/user-data/uploads/report.md", fake_client.last_message or "")
        self.assertIn("read_file", fake_client.last_message or "")

    async def test_handle_message_reports_resource_save_error_without_streaming(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "should not run", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        channel.resource_save_error = RuntimeError("download failed")
        resources = [
            _IncomingResource(
                resource_type="image",
                resource_key="img-key",
                filename="photo.png",
                message_id="user-message",
            )
        ]

        await channel.handle_message_for_test(fake_client, text="分析这张图", resources=resources)

        self.assertIsNone(fake_client.last_message)
        self.assertEqual(channel.patched_cards, [("card-1", "⚠️ 文件下载或保存失败，请稍后重试。\nRuntimeError: download failed")])
        self.assertEqual(channel.reactions, [("user-message", "OK"), ("user-message", "DONE")])

    async def test_handle_message_adds_done_reaction_after_stream_finishes(self) -> None:
        fake_client = _FakeClient(
            [
                StreamEvent(type="messages-tuple", data={"type": "ai", "content": "answer", "id": "ai-1", "is_delta": True}),
                StreamEvent(type="end", data={}),
            ]
        )
        channel = _RecordingFeishuChannel()
        channel.stream_started = asyncio.Event()
        channel.release_patch_card = asyncio.Event()
        handle_task = asyncio.create_task(channel.handle_message_for_test(fake_client))
        try:
            _ = await channel.stream_started.wait()
            await asyncio.sleep(0)

            self.assertIn(("user-message", "OK"), channel.reactions)
            self.assertNotIn(("user-message", "DONE"), channel.reactions)
            _ = channel.release_patch_card.set()
            await handle_task
        finally:
            if not handle_task.done():
                _ = handle_task.cancel()

        self.assertEqual(channel.reactions, [("user-message", "OK"), ("user-message", "DONE")])
        self.assertLess(channel.operation_log.index("patch:card-1:answer"), channel.operation_log.index("reaction:DONE"))

    async def test_pending_proposal_card_has_review_buttons_but_terminal_card_does_not(self) -> None:
        pending = json.loads(_make_proposal_card(_proposal(), "--- old\n+++ new"))
        published = json.loads(
            _make_proposal_card(
                _proposal(status="published"),
                "--- old\n+++ new",
            )
        )

        pending_action = next(element for element in pending["elements"] if element["tag"] == "action")
        values = [button["value"] for button in pending_action["actions"]]
        self.assertEqual(
            values,
            [
                {"channel": "deerflow_proposal", "action": "reject", "proposal_id": "p_test"},
                {"channel": "deerflow_proposal", "action": "approve", "proposal_id": "p_test"},
            ],
        )
        self.assertNotIn("action", [element["tag"] for element in published["elements"]])
        self.assertEqual(published["header"]["title"]["content"], "Skill Proposal · 已发布")

    async def test_proposals_command_sends_cards_without_streaming_agent(self) -> None:
        channel = _ProposalRecordingFeishuChannel()
        with patch(
            "app.proposal_review.list_pending_proposals",
            return_value=[(_proposal(), "diff")],
        ):
            await channel._handle_message(
                message_id="user-message",
                chat_id="chat-1",
                text="/proposals",
                thread_id="thread-1",
            )

        self.assertEqual(channel.stream_calls, 0)
        self.assertEqual(len(channel.sent_raw_cards), 1)
        self.assertEqual(channel.sent_cards, [])
        self.assertEqual(channel.reactions, [("user-message", "OK"), ("user-message", "DONE")])

    async def test_automatic_proposal_notifications_are_deduplicated_per_thread(self) -> None:
        channel = _ProposalRecordingFeishuChannel()
        with patch(
            "app.proposal_review.list_pending_proposals",
            return_value=[(_proposal(), "diff")],
        ):
            first = await channel._send_pending_proposal_cards("thread-1", "chat-1", force=False)
            second = await channel._send_pending_proposal_cards("thread-1", "chat-1", force=False)
            forced = await channel._send_pending_proposal_cards("thread-1", "chat-1", force=True)

        self.assertEqual((first, second, forced), (1, 0, 1))
        self.assertEqual(len(channel.sent_raw_cards), 2)

    async def test_approve_card_action_uses_shared_coordinator_and_patches_final_card(self) -> None:
        channel = _ProposalRecordingFeishuChannel()
        pending = _proposal()
        published = pending.model_copy(update={"status": "published", "published_revision": 2})
        approve = AsyncMock(return_value=published)
        with (
            patch("app.proposal_review.get_skill_proposal", return_value=(pending, "diff")),
            patch("app.proposal_review.approve_skill_proposal", approve),
        ):
            await channel._handle_proposal_action(
                card_id="message-card",
                proposal_id=pending.id,
                action="approve",
                actor="feishu:ou_user",
            )

        approve.assert_awaited_once_with(
            pending.id,
            expected_base_sha256=pending.base_sha256,
            note="在飞书中批准并发布",
            actor="feishu:ou_user",
        )
        self.assertEqual(len(channel.patched_raw_cards), 2)
        processing = json.loads(channel.patched_raw_cards[0][1])
        final = json.loads(channel.patched_raw_cards[1][1])
        self.assertEqual(processing["header"]["title"]["content"], "Skill Proposal · 处理中")
        self.assertEqual(final["header"]["title"]["content"], "Skill Proposal · 已发布")
        self.assertNotIn("action", [element["tag"] for element in final["elements"]])

    async def test_already_reviewed_card_action_only_refreshes_terminal_card(self) -> None:
        channel = _ProposalRecordingFeishuChannel()
        published = _proposal(status="published")
        approve = AsyncMock()
        with (
            patch("app.proposal_review.get_skill_proposal", return_value=(published, "diff")),
            patch("app.proposal_review.approve_skill_proposal", approve),
        ):
            await channel._handle_proposal_action(
                card_id="message-card",
                proposal_id=published.id,
                action="approve",
                actor="feishu:ou_user",
            )

        approve.assert_not_awaited()
        self.assertEqual(len(channel.patched_raw_cards), 1)
        final = json.loads(channel.patched_raw_cards[0][1])
        self.assertEqual(final["header"]["title"]["content"], "Skill Proposal · 已发布")

    async def test_invalid_proposal_action_payload_is_ignored(self) -> None:
        invalid = SimpleNamespace(
            event=SimpleNamespace(
                action=SimpleNamespace(
                    value={"channel": "other", "action": "approve", "proposal_id": "p_test"}
                ),
                context=SimpleNamespace(open_message_id="message-card"),
                operator=SimpleNamespace(open_id="ou_user"),
            )
        )
        valid = SimpleNamespace(
            event=SimpleNamespace(
                action=SimpleNamespace(
                    value={
                        "channel": "deerflow_proposal",
                        "action": "reject",
                        "proposal_id": "p_test",
                    }
                ),
                context=SimpleNamespace(open_message_id="message-card"),
                operator=SimpleNamespace(open_id="ou_user"),
            )
        )

        self.assertIsNone(_parse_proposal_action(invalid))
        self.assertEqual(
            _parse_proposal_action(valid),
            ("reject", "p_test", "message-card", "feishu:ou_user"),
        )

    async def test_astop_cancels_inflight_message_handlers(self) -> None:
        channel = _RecordingFeishuChannel()
        future: Future[None] = Future()
        with channel._handler_futures_lock:
            channel._handler_futures.add(future)

        await channel.astop()

        self.assertTrue(future.cancelled())
        self.assertEqual(channel._handler_futures, set())


if __name__ == "__main__":
    _ = unittest.main()
