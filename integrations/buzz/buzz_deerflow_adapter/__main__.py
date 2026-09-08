"""Command-line entry point."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
import sys
from contextlib import suppress

from .app import AdapterApp
from .config import parse_args


async def _run() -> None:
    config, once = parse_args()
    logging.basicConfig(
        level=getattr(logging, config.log_level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    app = AdapterApp(config)
    if once:
        await app.run_once()
        return

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, app._stop_requested.set)
        except (NotImplementedError, RuntimeError):
            pass
    pid_path = config.state_path.parent / "adapter.pid"
    await app.start()
    pid_path.parent.mkdir(parents=True, exist_ok=True)
    pid_path.write_text(str(os.getpid()), encoding="ascii")
    try:
        await app._stop_requested.wait()
    except KeyboardInterrupt:
        pass
    finally:
        await app.close()
        with suppress(OSError):
            if pid_path.read_text(encoding="ascii").strip() == str(os.getpid()):
                pid_path.unlink()


def main() -> None:
    try:
        asyncio.run(_run())
    except Exception as exc:
        print(f"buzz-deerflow-adapter: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc


if __name__ == "__main__":
    main()
