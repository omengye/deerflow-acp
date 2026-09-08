from __future__ import annotations

import asyncio
import uuid

import acp
from acp import schema


class FakeACPAgent:
    def __init__(self) -> None:
        self.connection = None

    def on_connect(self, connection) -> None:
        self.connection = connection

    async def initialize(self, protocol_version: int, **kwargs):
        del kwargs
        return acp.InitializeResponse(
            protocol_version=protocol_version,
            agent_capabilities=schema.AgentCapabilities(load_session=True),
            agent_info=schema.Implementation(
                name="fake-deerflow", title="Fake DeerFlow", version="1"
            ),
            auth_methods=[],
        )

    async def new_session(self, cwd: str, **kwargs):
        del cwd, kwargs
        return acp.NewSessionResponse(session_id=f"session-{uuid.uuid4().hex}")

    async def load_session(self, cwd: str, session_id: str, **kwargs):
        del cwd, session_id, kwargs
        return acp.LoadSessionResponse()

    async def prompt(self, prompt, session_id: str, **kwargs):
        del kwargs
        text = "\n".join(
            block.text for block in prompt if isinstance(block, schema.TextContentBlock)
        )
        assert self.connection is not None
        await self.connection.session_update(
            session_id=session_id,
            update=schema.AgentMessageChunk(
                session_update="agent_message_chunk",
                content=acp.text_block(f"Fake DeerFlow received:\n{text}"),
                message_id=str(uuid.uuid4()),
            ),
        )
        return acp.PromptResponse(stop_reason="end_turn")

    async def cancel(self, session_id: str, **kwargs):
        del session_id, kwargs


async def main() -> None:
    await acp.run_agent(FakeACPAgent(), use_unstable_protocol=False)


if __name__ == "__main__":
    asyncio.run(main())
