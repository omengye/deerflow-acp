from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path


def _value(args: list[str], flag: str, default: str = "") -> str:
    try:
        return args[args.index(flag) + 1]
    except (ValueError, IndexError):
        return default


def _load(path_name: str, default):
    path = os.getenv(path_name)
    if not path or not Path(path).exists():
        return default
    return json.loads(Path(path).read_text(encoding="utf-8"))


def _fail(category: str, message: str, *, retryable: bool, code: int) -> None:
    print(
        json.dumps({"error": category, "message": message, "retryable": retryable}),
        file=sys.stderr,
    )
    raise SystemExit(code)


def _record_operation(args: list[str]) -> None:
    path = os.getenv("FAKE_BUZZ_OPERATIONS")
    if path:
        with Path(path).open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(args) + "\n")


def main() -> None:
    args = sys.argv[1:]
    if args[:2] == ["channels", "list"]:
        print(json.dumps(_load("FAKE_BUZZ_CHANNELS", [])))
        return
    if args[:2] == ["dms", "list"]:
        print(json.dumps(_load("FAKE_BUZZ_DMS", [])))
        return
    if tuple(args[:2]) in {
        ("users", "set-profile"),
        ("users", "set-presence"),
        ("users", "set-status"),
        ("channels", "update"),
    }:
        _record_operation(args)
        print(json.dumps({"accepted": True}))
        return
    if args[:2] == ["messages", "get"]:
        if os.getenv("FAKE_BUZZ_GET_MODE") == "hang":
            time.sleep(5)
        if os.getenv("FAKE_BUZZ_GET_MODE") == "transport_error":
            _fail("network_error", "temporary network failure", retryable=True, code=2)
        channel = _value(args, "--channel")
        since = int(_value(args, "--since", "0"))
        before = int(_value(args, "--before", str(2**63 - 1)))
        limit = int(_value(args, "--limit", "200"))
        kinds = {
            int(value) for value in _value(args, "--kinds", "9").split(",") if value
        }
        events = [
            event
            for event in _load("FAKE_BUZZ_INBOX", [])
            if event.get("channel_id") == channel
            and int(event.get("created_at", 0)) >= since
            and int(event.get("created_at", 0)) <= before
            and int(event.get("kind", 0)) in kinds
        ]
        events.sort(key=lambda event: int(event.get("created_at", 0)))
        print(json.dumps(events[-limit:]))
        return
    if args[:2] == ["messages", "send"]:
        mode = os.getenv("FAKE_BUZZ_SEND_MODE", "")
        if mode == "hang":
            time.sleep(5)
        if mode == "delivery_unknown":
            _fail(
                "delivery_unknown",
                "relay may have stored the event",
                retryable=False,
                code=2,
            )
        if mode == "transport_error":
            _fail("network_error", "connect failed", retryable=True, code=2)
        content = sys.stdin.read()
        payload = {
            "channel_id": _value(args, "--channel"),
            "reply_to": _value(args, "--reply-to"),
            "content": content,
            "args": args,
        }
        sent_path = os.getenv("FAKE_BUZZ_SENT")
        if sent_path:
            with Path(sent_path).open("a", encoding="utf-8") as handle:
                handle.write(json.dumps(payload) + "\n")
        print(json.dumps({"event_id": "f" * 64, "accepted": True, "message": ""}))
        return
    if args[:2] == ["messages", "edit"]:
        _record_operation(args)
        print(json.dumps({"event_id": "e" * 64, "accepted": True, "message": ""}))
        return
    _fail("user_error", f"unsupported fake command: {args}", retryable=False, code=1)


if __name__ == "__main__":
    main()
