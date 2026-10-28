#!/usr/bin/env python3
"""Read-only Waku protocol/transport check; never start, prompt, cancel or save a task.

Requires websocket-client (already installed on the target Gentoo host). All other
imports use the Python standard library. TLS certificates are verified normally.
Only summaries are printed: no Token, settings values, IDs or conversation text.
"""

import argparse
import collections
import contextlib
import json
import os
import re
import socket
import sys
import time
import uuid
from urllib.parse import urlsplit


NIL_UUID = str(uuid.UUID(int=0))
READ_ONLY_COMMANDS = frozenset({
    "getSettings", "loadTaskState", "hydrateSession", "attachSession", "replayEvents",
})
KNOWN_STATUSES = frozenset({"idle", "connecting", "working", "waiting", "background", "error"})


class VerificationError(Exception):
    """Only controlled codes, never server error strings, enter the output."""

    def __init__(self, code, **details):
        super().__init__(code)
        self.code = code
        self.details = details


def require(condition, code):
    if not condition:
        raise VerificationError(code)


def valid_uuid(value):
    try:
        return isinstance(value, str) and str(uuid.UUID(value)) == value.lower()
    except (ValueError, AttributeError):
        return False


def validate_endpoint(endpoint):
    parsed = urlsplit(endpoint)
    require(parsed.scheme in ("ws", "wss") and parsed.hostname, "invalid_endpoint")
    require(parsed.path == "/v1" and not parsed.query and not parsed.fragment, "invalid_endpoint_path")
    require(parsed.username is None and parsed.password is None, "endpoint_credentials_not_allowed")
    try:
        parsed.port
    except ValueError:
        raise VerificationError("invalid_endpoint_port") from None
    return parsed


def websocket_factory(endpoint, timeout):
    try:
        import websocket
    except ImportError:
        raise VerificationError("websocket_client_dependency_missing") from None
    parsed = validate_endpoint(endpoint)
    # Match the Android native client: no browser Origin, and no inherited HTTP proxy.
    return websocket.create_connection(
        endpoint, timeout=timeout, suppress_origin=True,
        http_no_proxy=[parsed.hostname],
    )


class WakuProbe:
    def __init__(self, endpoint, token, protocol=10, timeout=15, factory=websocket_factory):
        self.endpoint = endpoint
        self.token = token
        self.protocol = protocol
        self.timeout = timeout
        self.factory = factory
        self.ws = None
        self.stage = "connect"
        self.ignored_events = 0

    def connect(self):
        self.stage = "connect"
        self.ws = self.factory(self.endpoint, self.timeout)
        self.stage = "hello"
        self.ws.send(json.dumps({
            "type": "hello", "protocolVersion": self.protocol,
            "token": self.token, "clientId": str(uuid.uuid4()), "resumeFrom": [],
        }))
        hello = self.receive(time.monotonic() + self.timeout)
        if hello.get("type") == "rejected":
            # Extract only numeric version information from the known protocol rejection.
            mismatch = re.fullmatch(
                r"protocol (\d+) is unsupported; expected (\d+)",
                str(hello.get("message", "")),
            )
            if mismatch:
                raise VerificationError("protocol_mismatch", requested=int(mismatch[1]), expected=int(mismatch[2]))
            raise VerificationError("handshake_rejected")
        require(hello.get("type") == "hello", "invalid_hello")
        require(hello.get("protocolVersion") == self.protocol, "unexpected_protocol_version")
        return {"ok": True, "protocolVersion": self.protocol}

    def receive(self, deadline):
        remaining = deadline - time.monotonic()
        require(remaining > 0, "response_deadline_exceeded")
        self.ws.settimeout(remaining)
        raw = self.ws.recv()
        require(isinstance(raw, str) and raw, "unexpected_or_closed_frame")
        require(len(raw) <= 48 * 1024 * 1024, "wire_message_too_large")
        try:
            decoded = json.loads(raw)
        except (ValueError, TypeError):
            raise VerificationError("invalid_json_frame") from None
        require(isinstance(decoded, dict), "invalid_message_shape")
        return decoded

    def rpc(self, command, session_id=NIL_UUID, runtime_id=NIL_UUID):
        kind = command.get("type")
        require(kind in READ_ONLY_COMMANDS, "mutation_command_blocked")
        require(valid_uuid(session_id) and valid_uuid(runtime_id), "invalid_request_identity")
        self.stage = kind
        request_id = str(uuid.uuid4())
        self.ws.send(json.dumps({
            "type": "request", "requestId": request_id,
            "sessionId": session_id, "runtimeId": runtime_id, "command": command,
        }))
        deadline = time.monotonic() + self.timeout
        for _ in range(4096):
            message = self.receive(deadline)
            require(message.get("type") != "shuttingDown", "server_shutting_down")
            if message.get("type") != "response" or message.get("requestId") != request_id:
                self.ignored_events += 1
                continue
            outcome = message.get("outcome")
            require(isinstance(outcome, dict), "invalid_response_outcome")
            require(outcome.get("status") == "ok", "read_only_rpc_rejected")
            payload = outcome.get("payload")
            require(isinstance(payload, dict), "invalid_response_payload")
            return payload
        raise VerificationError("response_event_limit_exceeded")

    def close(self, abrupt=False):
        if self.ws is None:
            return
        if abrupt:
            # Drop the transport without a WebSocket close frame; no server command is sent.
            transport = getattr(self.ws, "sock", None)
            if transport is not None:
                with contextlib.suppress(OSError):
                    transport.shutdown(socket.SHUT_RDWR)
                with contextlib.suppress(OSError):
                    transport.close()
        with contextlib.suppress(Exception):
            self.ws.close(timeout=1)
        self.ws = None


def validate_replay_page(page, session_id, runtime_id, after, epoch, through):
    require(page.get("type") == "replayEvents", "invalid_replay_response")
    page_epoch = page.get("epoch")
    require(valid_uuid(page_epoch), "invalid_replay_epoch")
    require(epoch is None or page_epoch == epoch, "replay_epoch_changed")
    upper, next_sequence, more = page.get("throughSequence"), page.get("nextSequence"), page.get("hasMore")
    require(type(upper) is int and type(next_sequence) is int and type(more) is bool, "invalid_replay_cursor_types")
    require(0 <= after <= next_sequence <= upper, "replay_cursor_out_of_range")
    require(through is None or upper == through, "replay_boundary_changed")
    events = page.get("events")
    require(isinstance(events, list), "invalid_replay_events")
    previous = after
    for event in events:
        require(isinstance(event, dict), "invalid_replay_event")
        # Replay pages contain SequencedEvent structs, without the live frame's type tag.
        require(event.get("replayable") is True and isinstance(event.get("event"), dict), "invalid_replay_event_payload")
        require(event.get("sessionId") == session_id and event.get("runtimeId") == runtime_id, "replay_identity_mismatch")
        require(event.get("epoch") == page_epoch, "replay_event_epoch_mismatch")
        sequence = event.get("sequence")
        require(type(sequence) is int and previous < sequence <= next_sequence, "replay_event_out_of_order")
        previous = sequence
    require(not more or (events and next_sequence > after and next_sequence == previous), "replay_not_advancing")
    require(more or next_sequence == upper, "incomplete_final_replay_page")
    return page_epoch, upper, next_sequence, more, len(events)


def inspect_once(probe, inspect_session=False, selected_id=None, max_pages=64):
    summary = {"hello": probe.connect()}
    settings = probe.rpc({"type": "getSettings"})
    require(settings.get("type") == "settings" and isinstance(settings.get("settings"), dict), "invalid_settings_response")
    summary["getSettings"] = {"ok": True}
    catalog = probe.rpc({"type": "loadTaskState"})
    require(catalog.get("type") == "taskState", "invalid_catalog_response")
    sessions, projects = catalog.get("sessions"), catalog.get("projects")
    require(isinstance(sessions, list) and isinstance(projects, list), "invalid_catalog_shape")
    require(all(isinstance(session, dict) for session in sessions), "invalid_session_summary")
    statuses = collections.Counter(
        session.get("status") if session.get("status") in KNOWN_STATUSES else "other"
        for session in sessions
    )
    summary["loadTaskState"] = {"ok": True, "sessions": len(sessions), "projects": len(projects), "statuses": dict(statuses)}
    if not inspect_session:
        return summary, None
    candidates = [session for session in sessions if session.get("provider") == "deerFlow"]
    chosen = next((session for session in candidates if session.get("id") == selected_id), None) if selected_id else next(iter(candidates), None)
    if chosen is None:
        require(selected_id is None, "requested_session_not_in_catalog")
        summary["sessionInspection"] = {"skipped": "no_existing_deerflow_session"}
        return summary, None
    session_id = chosen.get("id")
    require(valid_uuid(session_id), "invalid_catalog_session_id")
    hydrated = probe.rpc({"type": "hydrateSession", "sessionId": session_id})
    session = hydrated.get("session")
    require(isinstance(session, dict) and session.get("id") == session_id, "invalid_hydrated_session")
    messages, turns = session.get("messages"), session.get("turns")
    require(isinstance(messages, list) and isinstance(turns, list), "invalid_hydrated_history")
    summary["hydrateSession"] = {"ok": True, "messages": len(messages), "turns": len(turns)}
    attached = probe.rpc({"type": "attachSession"}, session_id)
    require(attached.get("type") == "sessionRuntime", "invalid_attach_response")
    runtime_id = attached.get("runtimeId")
    summary["attachSession"] = {"ok": True, "runtimePresent": runtime_id is not None}
    if runtime_id is None:
        summary["replayEvents"] = {"skipped": "no_live_runtime"}
        return summary, session_id
    require(valid_uuid(runtime_id), "invalid_runtime_id")
    after, epoch, through, event_count = 0, None, None, 0
    for page_number in range(1, max_pages + 1):
        page = probe.rpc({
            "type": "replayEvents", "epoch": epoch, "afterSequence": after,
            "throughSequence": through, "limit": 256,
        }, session_id, runtime_id)
        epoch, through, after, more, count = validate_replay_page(page, session_id, runtime_id, after, epoch, through)
        event_count += count
        if not more:
            summary["replayEvents"] = {"ok": True, "complete": True, "pages": page_number, "events": event_count}
            return summary, session_id
    summary["replayEvents"] = {"ok": True, "complete": False, "bounded": True, "pages": max_pages, "events": event_count}
    return summary, session_id


def verify(args, token, factory=websocket_factory):
    validate_endpoint(args.endpoint)
    require(bool(token.strip()), "token_env_missing_or_empty")
    require(args.protocol > 0 and args.timeout > 0 and 1 <= args.reconnect_cycles <= 10 and 1 <= args.max_pages <= 1024, "invalid_probe_limits")
    require(args.session_id is None or valid_uuid(args.session_id), "invalid_selected_session_id")
    report = {"ok": False, "endpoint": args.endpoint, "protocolVersion": args.protocol, "cycles": []}
    selected = args.session_id
    for cycle in range(args.reconnect_cycles):
        probe = WakuProbe(args.endpoint, token, args.protocol, args.timeout, factory)
        try:
            summary, selected = inspect_once(probe, args.inspect_session or selected is not None, selected, args.max_pages)
            summary["ignoredLiveMessages"] = probe.ignored_events
            report["cycles"].append(summary)
        except VerificationError as error:
            report["failure"] = {"stage": probe.stage, "code": error.code, **error.details}
            return report
        except Exception as error:
            # Do not print exception messages: network/server errors can include credentials or bodies.
            report["failure"] = {"stage": probe.stage, "code": "transport_or_decode_error", "exceptionType": type(error).__name__}
            return report
        finally:
            probe.close(abrupt=args.abrupt_first_disconnect and cycle == 0)
    report["ok"] = True
    report["reconnectVerified"] = args.reconnect_cycles > 1
    report["abruptFirstDisconnect"] = args.abrupt_first_disconnect
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--endpoint", required=True)
    parser.add_argument("--token-env", default="WAKU_DAEMON_TOKEN")
    parser.add_argument("--protocol", type=int, default=10)
    parser.add_argument("--timeout", type=float, default=15)
    parser.add_argument("--inspect-session", action="store_true")
    parser.add_argument("--session-id", default=None)
    parser.add_argument("--max-pages", type=int, default=64)
    parser.add_argument("--reconnect-cycles", type=int, default=2)
    parser.add_argument("--abrupt-first-disconnect", action="store_true")
    args = parser.parse_args(argv)
    try:
        report = verify(args, os.environ.get(args.token_env, ""))
    except VerificationError as error:
        report = {"ok": False, "failure": {"stage": "preflight", "code": error.code, **error.details}}
    except Exception as error:
        report = {"ok": False, "failure": {"stage": "preflight", "code": "preflight_error", "exceptionType": type(error).__name__}}
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
