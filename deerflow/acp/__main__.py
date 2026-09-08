"""Run the local DeerFlow ACP agent over stdin/stdout."""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys
from pathlib import Path

# ACP clients often start the process in the user's open workspace.  Move to
# DeerFlow's own project root before importing any DeerFlow runtime module;
# some upstream configuration modules perform .env discovery at import time.
_PROJECT_ROOT = Path(__file__).resolve().parents[2]
os.chdir(_PROJECT_ROOT)

import acp

from .config import LocalACPConfig


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Run DeerFlow as a local ACP v1 stdio agent")
    parser.add_argument("--config", help="Path to DeerFlow config.yaml")
    return parser


async def _run(config_path: str | None) -> None:
    config = LocalACPConfig.from_file(config_path)
    config.prepare_environment()
    # Importing the embedded runtime is intentionally deferred until the
    # isolated environment paths above have been installed.
    from .agent import DeerFlowACPAgent
    from .runtime import LocalACPRuntime
    from .session_cleanup import cleanup_expired_sessions, run_session_cleanup_loop
    from .session_store import LocalACPSessionStore

    runtime = LocalACPRuntime(config)
    runtime.validate_sandbox_provider()
    store = LocalACPSessionStore(config.session_store_path)
    store.setup()
    await runtime.open()
    await cleanup_expired_sessions(config, store, runtime, compact=True)
    cleanup_task = asyncio.create_task(
        run_session_cleanup_loop(config, store, runtime)
    )
    agent = DeerFlowACPAgent(config, store, runtime)
    try:
        # Keep the wire surface on stable ACP v1. Experimental workspace,
        # terminal, fork/resume, and provider features remain unavailable.
        await acp.run_agent(agent, use_unstable_protocol=False)
    finally:
        cleanup_task.cancel()
        try:
            await cleanup_task
        except asyncio.CancelledError:
            pass
        await agent.shutdown()
        await runtime.close()
        store.close()


def main() -> None:
    args = _parser().parse_args()
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        stream=sys.stderr,
    )
    try:
        asyncio.run(_run(args.config))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
