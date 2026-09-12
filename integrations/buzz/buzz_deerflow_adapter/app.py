"""Long-running orchestration loop for Buzz and DeerFlow ACP."""

from __future__ import annotations

import asyncio
import logging
from contextlib import suppress

from .acp_client import DeerFlowACPClient
from .acp_v2_client import DeerFlowACPV2Client
from .attachments import AttachmentError, parse_attachments, prepare_attachments
from .buzz_cli import (
    BuzzCLI,
    BuzzCLIError,
    BuzzDeliveryUnknownError,
    BuzzTransportError,
)
from .config import AdapterConfig
from .models import BuzzChannel, BuzzMessage, PendingMessage
from .observer import NipAOObserver, ObserverConfigError
from .progress import ToolProgressReporter
from .state import AdapterState

logger = logging.getLogger(__name__)


class AdapterApp:
    def __init__(self, config: AdapterConfig) -> None:
        self.config = config
        self.state = AdapterState(config.state_path)
        self.buzz = BuzzCLI(
            config.buzz_command,
            config.buzz_args,
            config.relay_url,
            timeout_seconds=config.buzz_cli_timeout_seconds,
        )
        acp_client = (
            DeerFlowACPV2Client
            if config.deerflow_protocol == "v2"
            else DeerFlowACPClient
        )
        self.acp = acp_client(
            config.deerflow_command,
            config.deerflow_args,
            config.workspace,
            timeout_seconds=config.acp_timeout_seconds,
        )
        self._stop_requested = asyncio.Event()
        self._task: asyncio.Task[None] | None = None
        self._presence_task: asyncio.Task[None] | None = None
        self._lifecycle_lock = asyncio.Lock()
        self._transport_failure_streak = 0
        self.observer: NipAOObserver | None = None
        if config.observer_enabled:
            try:
                self.observer = NipAOObserver.from_environment(
                    relay_url=config.relay_url,
                    agent_pubkey=config.agent_pubkey,
                    owner_pubkey=config.observer_owner_pubkey,
                    queue_size=config.observer_queue_size,
                    publish_timeout_seconds=config.observer_publish_timeout_seconds,
                    include_raw_tool_data=config.observer_include_raw_tool_data,
                )
            except ObserverConfigError as exc:
                raise ValueError(
                    f"Buzz observer configuration is invalid: {exc}"
                ) from exc
            if self.observer is None:
                logger.warning(
                    "Buzz native observer is enabled but no owner pubkey is available; "
                    "set BUZZ_AUTH_TAG or observability.owner_pubkey. Final replies "
                    "and lifecycle status will continue without tool telemetry"
                )

    async def start(self) -> None:
        await self.acp.open()
        if self.observer is not None:
            await self.observer.open()
        await self._announce_started()
        if self.config.presence_enabled:
            self._presence_task = asyncio.create_task(
                self._presence_heartbeat(), name="buzz-presence-heartbeat"
            )
        self._task = asyncio.create_task(self._poll_loop(), name="buzz-inbox-poll")

    async def run(self) -> None:
        await self.start()
        try:
            await self._stop_requested.wait()
        finally:
            await self.close()

    async def close(self) -> None:
        self._stop_requested.set()
        task, self._task = self._task, None
        if task is not None:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        presence_task, self._presence_task = self._presence_task, None
        if presence_task is not None:
            presence_task.cancel()
            await asyncio.gather(presence_task, return_exceptions=True)
        await self.acp.close()
        await self._announce_stopped()
        if self.observer is not None:
            await self.observer.close()
        self.state.close()

    async def run_once(self) -> None:
        await self.acp.open()
        if self.observer is not None:
            await self.observer.open()
        await self._announce_started()
        try:
            await self.drain_once()
        finally:
            await self.acp.close()
            await self._announce_stopped()
            if self.observer is not None:
                await self.observer.close()
            self.state.close()

    async def _best_effort_buzz_write(self, label: str, operation) -> bool:
        try:
            async with asyncio.timeout(min(self.config.buzz_cli_timeout_seconds, 10)):
                async with self._lifecycle_lock:
                    await self._with_transport_retry(operation)
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # noqa: BLE001 - metadata must not stop replies
            logger.warning("Could not update Buzz %s: %s", label, exc)
            return False
        return True

    async def _announce_started(self) -> None:
        if any(
            (
                self.config.profile_name,
                self.config.profile_avatar,
                self.config.profile_about,
                self.config.profile_nip05,
            )
        ):
            await self._best_effort_buzz_write(
                "profile",
                lambda: self.buzz.set_profile(
                    name=self.config.profile_name,
                    avatar=self.config.profile_avatar,
                    about=self.config.profile_about,
                    nip05=self.config.profile_nip05,
                ),
            )
        current_channel_names: dict[str, str] = {}
        if self.config.channel_names:
            try:
                channels = await self._with_transport_retry(self.buzz.list_channels)
                current_channel_names = {
                    channel.channel_id: channel.name for channel in channels
                }
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 - a rename is best effort
                logger.debug("Could not read Buzz channel names before update: %s", exc)
        for channel_id, name in self.config.channel_names.items():
            if current_channel_names.get(channel_id) == name:
                continue
            await self._best_effort_buzz_write(
                f"channel {channel_id} name",
                lambda channel_id=channel_id, name=name: self.buzz.update_channel_name(
                    channel_id, name
                ),
            )
        if self.config.presence_enabled:
            await self._best_effort_buzz_write(
                "presence", lambda: self._set_presence("online")
            )
        await self._set_activity_status(self.config.status_ready)

    async def _announce_stopped(self) -> None:
        if self.config.status_enabled:
            await self._best_effort_buzz_write(
                "status", lambda: self.buzz.set_status(clear=True)
            )
        if self.config.presence_enabled:
            await self._best_effort_buzz_write(
                "presence", lambda: self._set_presence("offline")
            )

    async def _set_presence(self, status: str) -> None:
        if self.observer is not None:
            try:
                await self.observer.set_presence(status)
                return
            except Exception as exc:  # noqa: BLE001 - CLI presence is the fallback
                logger.warning(
                    "Buzz native observer presence failed; falling back to CLI: %s",
                    exc,
                )
        await self.buzz.set_presence(status)

    async def _set_activity_status(self, text: str) -> None:
        if not self.config.status_enabled:
            return
        await self._best_effort_buzz_write(
            "status",
            lambda: self.buzz.set_status(text, emoji=self.config.status_emoji),
        )

    async def _presence_heartbeat(self) -> None:
        while not self._stop_requested.is_set():
            try:
                await asyncio.wait_for(
                    self._stop_requested.wait(),
                    timeout=self.config.presence_heartbeat_seconds,
                )
                return
            except TimeoutError:
                await self._best_effort_buzz_write(
                    "presence heartbeat", lambda: self._set_presence("online")
                )

    async def _poll_loop(self) -> None:
        while not self._stop_requested.is_set():
            try:
                await self.drain_once()
            except asyncio.CancelledError:
                raise
            except BuzzTransportError as exc:
                self._transport_failure_streak += 1
                log = (
                    logger.error
                    if self._transport_failure_streak >= 3
                    else logger.warning
                )
                log(
                    "Buzz transport unavailable (failed poll cycles=%d); "
                    "will retry: %s",
                    self._transport_failure_streak,
                    exc,
                )
            except Exception:
                logger.exception("Buzz inbox poll failed")
            else:
                if self._transport_failure_streak:
                    logger.info(
                        "Buzz transport recovered after %d failed poll cycle(s)",
                        self._transport_failure_streak,
                    )
                    self._transport_failure_streak = 0
            try:
                await asyncio.wait_for(
                    self._stop_requested.wait(),
                    timeout=self.config.poll_interval_seconds,
                )
            except TimeoutError:
                pass

    async def _with_transport_retry(self, operation):
        attempts = self.config.transport_retry_attempts
        delay = self.config.transport_retry_base_seconds
        for attempt in range(1, attempts + 1):
            try:
                return await operation()
            except asyncio.CancelledError:
                raise
            except BuzzTransportError:
                if attempt >= attempts:
                    raise
                logger.warning(
                    "Buzz transport failed (attempt %d/%d); retrying in %.1fs",
                    attempt,
                    attempts,
                    delay,
                )
                if delay:
                    await asyncio.sleep(delay)
                delay = min(
                    max(delay * 2, self.config.transport_retry_base_seconds),
                    self.config.transport_retry_max_seconds,
                )
        raise AssertionError("unreachable")

    async def _discover_channels(self) -> list[BuzzChannel]:
        channels: dict[str, BuzzChannel] = {
            channel_id: BuzzChannel(channel_id=channel_id)
            for channel_id in self.config.channels
        }
        if self.config.auto_discover_channels:
            discovered = await self._with_transport_retry(self.buzz.list_channels)
            channels.update({channel.channel_id: channel for channel in discovered})
        if self.config.include_dms:
            dms = await self._with_transport_retry(self.buzz.list_dms)
            # DM knowledge wins if it is also present in general channel discovery.
            channels.update({channel.channel_id: channel for channel in dms})
        return sorted(channels.values(), key=lambda channel: channel.channel_id)

    async def drain_once(self) -> None:
        await self._process_pending()
        for channel in await self._discover_channels():
            try:
                await self._drain_channel(channel)
            except BuzzTransportError:
                raise
            except BuzzCLIError:
                logger.exception(
                    "Skipping Buzz channel %s after a non-retryable CLI error",
                    channel.channel_id,
                )
        await self._process_pending()

    async def _read_channel_messages(
        self, channel: BuzzChannel, *, since: int | None
    ) -> list[BuzzMessage]:
        found: dict[str, BuzzMessage] = {}
        before: int | None = None
        page_size = 200
        for page_number in range(1, self.config.max_poll_pages + 1):
            page = await self._with_transport_retry(
                lambda before=before: self.buzz.get_messages(
                    channel,
                    since=since,
                    before=before,
                    limit=page_size,
                    kinds=self.config.message_kinds,
                )
            )
            found.update({message.event_id: message for message in page})
            if len(page) < page_size:
                break
            oldest = min(message.created_at for message in page)
            next_before = oldest - 1
            if oldest <= 0 or (since is not None and next_before < since):
                break
            before = next_before
            if page_number == self.config.max_poll_pages:
                raise BuzzCLIError(
                    f"Buzz channel {channel.channel_id} has more messages than "
                    f"adapter.max_poll_pages={self.config.max_poll_pages} can read "
                    "in one pass; its cursor was not advanced. Increase the limit "
                    "and restart the adapter"
                )
        return sorted(
            found.values(), key=lambda message: (message.created_at, message.event_id)
        )

    async def _drain_channel(self, channel: BuzzChannel) -> None:
        cursor = self.state.get_cursor(channel.channel_id)
        if cursor is None and not self.config.replay_existing:
            latest = await self._with_transport_retry(
                lambda: self.buzz.get_messages(
                    channel, limit=1, kinds=self.config.message_kinds
                )
            )
            seeded_at = max((message.created_at for message in latest), default=0)
            self.state.put_cursor(channel.channel_id, seeded_at)
            logger.info(
                "Initialized Buzz channel %s at cursor %d without replay",
                channel.channel_id,
                seeded_at,
            )
            return

        since = None if cursor is None else max(0, cursor - 1)
        messages = await self._read_channel_messages(channel, since=since)
        eligible = [message for message in messages if self._eligible(message)]
        inserted = self.state.enqueue(eligible, session_scope=self.config.session_scope)
        if messages:
            self.state.put_cursor(
                channel.channel_id,
                max(message.created_at for message in messages),
            )
        if eligible:
            logger.info(
                "Read %d Buzz event(s) from %s, %d eligible, %d new",
                len(messages),
                channel.channel_id,
                len(eligible),
                inserted,
            )

    def _eligible(self, message: BuzzMessage) -> bool:
        if message.author_pubkey.casefold() == self.config.agent_pubkey.casefold():
            return False
        has_attachments = any(tag and tag[0] == "imeta" for tag in message.tags)
        if message.kind not in self.config.message_kinds or not (
            message.content.strip() or has_attachments
        ):
            return False
        allowed = {value.casefold() for value in self.config.allowed_pubkeys}
        if allowed and message.author_pubkey.casefold() not in allowed:
            return False
        if not message.is_dm and self.config.require_mention:
            return message.mentions(self.config.agent_pubkey)
        return True

    async def _process_pending(self) -> None:
        for message in self.state.pending():
            try:
                await self._process_message(message)
            except asyncio.CancelledError:
                raise
            except BuzzDeliveryUnknownError as exc:
                self.state.mark_delivery_unknown(message.key, str(exc))
                logger.error(
                    "Buzz delivery state is unknown for event %s; quarantined "
                    "to avoid a duplicate reply",
                    message.event_id,
                )
            except Exception as exc:
                exhausted = self.state.mark_failed(
                    message.key,
                    f"{type(exc).__name__}: {exc}",
                    max_attempts=self.config.max_message_attempts,
                )
                logger.exception("Failed to process Buzz event %s", message.event_id)
                if exhausted:
                    logger.error(
                        "Buzz event %s reached the retry limit (%d)",
                        message.event_id,
                        self.config.max_message_attempts,
                    )
                if not isinstance(exc, BuzzCLIError):
                    with suppress(Exception):
                        await self.acp.close()
                        await self.acp.open()

    async def _process_message(self, message: PendingMessage) -> None:
        response = message.response_content
        if response is None:
            existing = self.state.get_session(
                message.conversation_key, self.config.workspace
            )
            session_id = await self.acp.attach_or_create(existing)
            if session_id != existing:
                self.state.put_session(
                    message.conversation_key, session_id, self.config.workspace
                )
            await self._set_activity_status(self.config.status_working)
            progress = (
                ToolProgressReporter(
                    self.buzz,
                    channel_id=message.channel_id,
                    reply_to=message.event_id,
                    update_interval_seconds=(
                        self.config.progress_update_interval_seconds
                    ),
                    max_tools=self.config.progress_max_tools,
                )
                if self.config.progress_messages_enabled
                else None
            )
            if self.observer is not None:
                self.observer.turn_started(
                    message.channel_id, session_id, message.event_id
                )

            def observe_update(update):
                if self.observer is not None:
                    self.observer.acp_update(
                        message.channel_id,
                        session_id,
                        message.event_id,
                        update,
                    )
                if progress is not None:
                    progress.on_update(update)

            try:
                prompt = self._build_prompt(message)
                attachments = parse_attachments(message.tags)
                if attachments:
                    if self.config.deerflow_protocol != "v2":
                        raise AttachmentError(
                            "Buzz attachment input requires deerflow.protocol = 'v2'"
                        )
                    if (
                        any(item.is_image for item in attachments)
                        and not self.acp.supports_images
                    ):
                        raise AttachmentError(
                            "DeerFlow must advertise ACP v2 image support; configure a vision model and update the bridge"
                        )
                    blocks = await prepare_attachments(
                        attachments,
                        buzz=self.buzz,
                        workspace=self.config.workspace,
                        session_id=session_id,
                        event_id=message.event_id,
                    )
                    prompt = [{"type": "text", "text": prompt}, *blocks]
                if self.observer is not None or progress is not None:
                    response = await self.acp.prompt(
                        session_id,
                        prompt,
                        on_update=observe_update,
                    )
                else:
                    response = await self.acp.prompt(session_id, prompt)
            except Exception as exc:
                if self.observer is not None:
                    self.observer.session_resolved(
                        message.channel_id,
                        session_id,
                        message.event_id,
                        outcome="error",
                        detail=str(exc),
                    )
                await self._set_activity_status(self.config.status_error)
                if progress is not None:
                    await progress.finish("error")
                if isinstance(exc, AttachmentError):
                    # Invalid attachments cannot improve on retry. Persist a useful
                    # reply through the normal outbox instead of silently dropping them.
                    response = f"DeerFlow could not process the attachments: {exc}"
                else:
                    raise
            else:
                if self.observer is not None:
                    self.observer.session_resolved(
                        message.channel_id,
                        session_id,
                        message.event_id,
                        outcome="ok",
                    )
                await self._set_activity_status(self.config.status_ready)
                if progress is not None:
                    await progress.finish("ok")
            if not response:
                response = "DeerFlow completed the turn without a text response."
            # Persist the exact reply before network delivery. A safe retry reuses
            # this body and never reruns the ACP turn.
            self.state.save_response(message.key, response)
        await self._with_transport_retry(
            lambda: self.buzz.send_message(
                message.channel_id, message.event_id, response
            )
        )
        self.state.mark_done(message.key)
        logger.info(
            "Replied to Buzz event %s in channel %s",
            message.event_id,
            message.channel_id,
        )

    @staticmethod
    def _build_prompt(message: PendingMessage) -> str:
        surface = "direct message" if message.is_dm else "channel message"
        return (
            "You are responding through a Buzz-to-DeerFlow adapter. Produce only "
            "the final reply that the adapter should post back to Buzz. The adapter "
            "handles delivery; do not try to invoke the Buzz CLI yourself.\n\n"
            f"Buzz surface: {surface}\n"
            f"Buzz channel id: {message.channel_id}\n"
            f"Buzz sender pubkey: {message.author_pubkey}\n"
            f"Buzz event id: {message.event_id}\n\n"
            "Message:\n"
            f"{message.content}"
        )
