"""Prewarm one DeerFlow Desktop ACP bundle at Windows user logon.

The registration script installs this file in LOCALAPPDATA and passes the
bundle directory explicitly. Restart registration after moving or upgrading
the bundle so a previous profile is never started by accident.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time


CREATE_NO_WINDOW = 0x08000000 if os.name == "nt" else 0
REQUEST = b'{"operation":"daemon.status"}\n'


def prewarm(root: Path, timeout_seconds: int) -> tuple[str, str]:
    if not root.is_dir():
        return "failed", "bundle_missing"

    bridge = root / "deerflow-acp.exe"
    python = root / "runtime" / "python.exe"
    config = root / "user-data" / "config" / "config.yaml"
    runtime = root / "user-data" / "runtime" / "acp"
    if not all(path.is_file() for path in (bridge, python, config)):
        return "failed", "bundle_incomplete"

    common = [
        "--config", str(config),
        "--python", str(python),
        "--runtime-dir", str(runtime),
    ]
    try:
        started = subprocess.run(
            [str(bridge), "--start-daemon", *common],
            cwd=root,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=150,
            creationflags=CREATE_NO_WINDOW,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return "failed", "bridge_start_failed"
    if started.returncode != 0:
        return "failed", "bridge_start_failed"

    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        try:
            response = subprocess.run(
                [str(bridge), "--manage", *common],
                cwd=root,
                input=REQUEST,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                timeout=35,
                creationflags=CREATE_NO_WINDOW,
                check=False,
            )
            if response.returncode == 0:
                envelope = json.loads(response.stdout)
                if envelope.get("ok") is True:
                    warmup = envelope.get("data", {}).get("warmup")
                    if warmup == "ready":
                        return "ready", "warmup_ready"
                    if warmup == "failed":
                        return "failed", "warmup_failed"
        except (OSError, subprocess.TimeoutExpired, ValueError, AttributeError):
            pass
        time.sleep(0.75)
    return "failed", "warmup_timeout"


def write_status(state: str, reason: str, started: dt.datetime, elapsed_ms: int) -> None:
    local_app_data = os.environ.get("LOCALAPPDATA")
    if not local_app_data:
        return
    directory = Path(local_app_data) / "DeerFlow" / "prewarm"
    directory.mkdir(parents=True, exist_ok=True)
    finished = dt.datetime.now(dt.timezone.utc)
    payload = {
        "state": state,
        "reason": reason,
        "started_utc": started.isoformat(),
        "finished_utc": finished.isoformat(),
        "elapsed_ms": elapsed_ms,
    }
    destination = directory / "last-run.json"
    with tempfile.NamedTemporaryFile(
        mode="w", encoding="utf-8", dir=directory, prefix="last-run-", suffix=".tmp", delete=False
    ) as stream:
        json.dump(payload, stream, ensure_ascii=False, separators=(",", ":"))
        temporary = Path(stream.name)
    try:
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle-root", type=Path, required=True)
    parser.add_argument("--warmup-timeout-seconds", type=int, default=180)
    args = parser.parse_args()

    started = dt.datetime.now(dt.timezone.utc)
    tick = time.monotonic()
    try:
        if not 10 <= args.warmup_timeout_seconds <= 600:
            state, reason = "failed", "invalid_timeout"
        else:
            state, reason = prewarm(args.bundle_root.absolute(), args.warmup_timeout_seconds)
    except Exception:
        # The status file deliberately omits exception text, paths, and secrets.
        state, reason = "failed", "unexpected_error"
    try:
        write_status(state, reason, started, round((time.monotonic() - tick) * 1000))
    except OSError:
        pass
    return 0 if state == "ready" else 1


if __name__ == "__main__":
    sys.exit(main())
