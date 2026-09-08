"""Coalesced, public tool progress message for identities without NIP-AO."""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from typing import Any

from .buzz_cli import BuzzCLI, BuzzDeliveryUnknownError

logger = logging.getLogger(__name__)


@dataclass(slots=True)
class _ToolState:
    title: str
    status: str


class ToolProgressReporter:
    """Maintains at most one editable thread reply containing tool status."""

    def __init__(
        self,
        buzz: BuzzCLI,
        *,
        channel_id: str,
        reply_to: str,
        update_interval_seconds: float = 0.5,
        max_tools: int = 12,
    ) -> None:
        self.buzz = buzz
        self.channel_id = channel_id
        self.reply_to = reply_to
        self.update_interval_seconds = update_interval_seconds
        self.max_tools = max_tools
        self._tools: dict[str, _ToolState] = {}
        self._queue: asyncio.Queue[dict[str, Any] | None] = asyncio.Queue()
        self._event_id = ""
        self._disabled = False
        self._task = asyncio.create_task(
            self._worker(), name=f"buzz-tool-progress-{reply_to[:8]}"
        )

    def on_update(self, update: dict[str, Any]) -> None:
        if update.get("sessionUpdate") in {"tool_call", "tool_call_update"}:
            self._queue.put_nowait(dict(update))

    async def finish(self, outcome: str) -> None:
        self._queue.put_nowait(None)
        try:
            async with asyncio.timeout(3):
                await self._task
        except TimeoutError:
            self._task.cancel()
            await asyncio.gather(self._task, return_exceptions=True)
            logger.warning("Timed out finalizing Buzz tool progress message")
        if self._event_id and not self._disabled:
            heading = (
                "✅ DeerFlow 工具调用完成"
                if outcome == "ok"
                else "❌ DeerFlow 工具调用中断"
            )
            await self._publish(self._render(heading))

    async def _worker(self) -> None:
        while True:
            update = await self._queue.get()
            if update is None:
                return
            self._apply(update)
            if self.update_interval_seconds:
                await asyncio.sleep(self.update_interval_seconds)
            while True:
                try:
                    pending = self._queue.get_nowait()
                except asyncio.QueueEmpty:
                    break
                if pending is None:
                    await self._publish(self._render("🔧 DeerFlow 工具调用"))
                    return
                self._apply(pending)
            await self._publish(self._render("🔧 DeerFlow 工具调用"))

    def _apply(self, update: dict[str, Any]) -> None:
        tool_id = update.get("toolCallId")
        if not isinstance(tool_id, str) or not tool_id:
            return
        current = self._tools.get(tool_id)
        title = update.get("title")
        if not isinstance(title, str) or not title:
            title = current.title if current is not None else tool_id
        status = update.get("status")
        if not isinstance(status, str) or not status:
            status = current.status if current is not None else "pending"
        self._tools[tool_id] = _ToolState(title=title[:160], status=status)

    def _render(self, heading: str) -> str:
        icons = {
            "pending": "⏳",
            "in_progress": "🔄",
            "completed": "✅",
            "failed": "❌",
            "cancelled": "⏹️",
        }
        items = list(self._tools.values())
        lines = [heading]
        for tool in items[: self.max_tools]:
            lines.append(f"{icons.get(tool.status, '•')} {tool.title}")
        if len(items) > self.max_tools:
            lines.append(f"…另有 {len(items) - self.max_tools} 个工具调用")
        return "\n".join(lines)

    async def _publish(self, content: str) -> None:
        if self._disabled or not self._tools:
            return
        try:
            async with asyncio.timeout(3):
                if self._event_id:
                    await self.buzz.edit_message(self._event_id, content)
                    return
                result = await self.buzz.send_message(
                    self.channel_id, self.reply_to, content
                )
            event_id = result.get("event_id") or result.get("id")
            if not isinstance(event_id, str) or not event_id:
                raise RuntimeError("Buzz progress reply did not return an event id")
            self._event_id = event_id
        except BuzzDeliveryUnknownError:
            self._disabled = True
            logger.warning(
                "Buzz tool progress delivery is unknown; disabling edits for this turn"
            )
        except Exception as exc:  # noqa: BLE001 - progress is best effort
            self._disabled = True
            logger.warning("Buzz tool progress disabled for this turn: %s", exc)
