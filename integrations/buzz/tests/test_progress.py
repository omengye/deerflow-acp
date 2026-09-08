from __future__ import annotations

from buzz_deerflow_adapter.progress import ToolProgressReporter


class _FakeBuzz:
    def __init__(self) -> None:
        self.sent: list[str] = []
        self.edited: list[str] = []

    async def send_message(self, _channel: str, _reply_to: str, content: str):
        self.sent.append(content)
        return {"event_id": "f" * 64, "accepted": True}

    async def edit_message(self, _event_id: str, content: str):
        self.edited.append(content)
        return {"accepted": True}


async def test_progress_coalesces_tool_updates_into_one_editable_message() -> None:
    buzz = _FakeBuzz()
    reporter = ToolProgressReporter(
        buzz,  # type: ignore[arg-type]
        channel_id="channel",
        reply_to="c" * 64,
        update_interval_seconds=0,
    )
    reporter.on_update(
        {
            "sessionUpdate": "tool_call",
            "toolCallId": "tool-1",
            "title": "web_search",
            "status": "pending",
            "rawInput": {"query": "private"},
        }
    )
    reporter.on_update(
        {
            "sessionUpdate": "tool_call_update",
            "toolCallId": "tool-1",
            "status": "completed",
            "content": [{"type": "text", "text": "private result"}],
        }
    )
    await reporter.finish("ok")

    assert len(buzz.sent) == 1
    assert buzz.edited
    assert "web_search" in buzz.edited[-1]
    assert "private" not in "\n".join([*buzz.sent, *buzz.edited])
    assert buzz.edited[-1].startswith("✅")
