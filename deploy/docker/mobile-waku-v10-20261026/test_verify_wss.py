import json
import unittest
from types import SimpleNamespace

import verify_wss as probe


SESSION = "11111111-1111-4111-8111-111111111111"
RUNTIME = "22222222-2222-4222-8222-222222222222"
EPOCH = "33333333-3333-4333-8333-333333333333"
TOKEN = "test-only-private-token"


def event(sequence):
    return {"sessionId": SESSION, "runtimeId": RUNTIME, "epoch": EPOCH,
            "sequence": sequence, "replayable": True,
            "event": {"kind": "textDelta", "payload": "private conversation content"}}


def page(events, through, next_sequence, more):
    return {"type": "replayEvents", "epoch": EPOCH, "events": events,
            "throughSequence": through, "nextSequence": next_sequence, "hasMore": more}


class FakeSocket:
    def __init__(self, runtime=None, hello=None, fail_rpc=False):
        self.runtime = runtime
        self.hello = hello or {"type": "hello", "protocolVersion": 10}
        self.fail_rpc = fail_rpc
        self.sent = []
        self.responses = []
        self.closed = False
        self.sock = None

    def send(self, raw):
        wire = json.loads(raw)
        self.sent.append(wire)
        if wire["type"] == "hello":
            self.responses.append(self.hello)
            return
        command = wire["command"]
        kind = command["type"]
        if kind == "getSettings":
            payload = {"type": "settings", "settings": {"private": TOKEN}}
        elif kind == "loadTaskState":
            payload = {"type": "taskState", "projects": [{}], "sessions": [
                {"id": SESSION, "provider": "deerFlow", "status": "idle"}]}
        elif kind == "hydrateSession":
            payload = {"type": "hydratedSession", "session": {
                "id": SESSION, "messages": [{"content": "private conversation content"}], "turns": [{}]}}
        elif kind == "attachSession":
            payload = {"type": "sessionRuntime", "runtimeId": self.runtime}
        elif kind == "replayEvents":
            payload = page([event(1)], 3, 1, True) if command["afterSequence"] == 0 else page([event(3)], 3, 3, False)
        else:
            raise AssertionError("unexpected command")
        outcome = {"status": "ok", "payload": payload}
        if self.fail_rpc:
            outcome = {"status": "error", "error": {"message": TOKEN + " private conversation content"}}
        self.responses.append({"type": "response", "requestId": wire["requestId"], "outcome": outcome})

    def recv(self):
        return json.dumps(self.responses.pop(0))

    def settimeout(self, timeout):
        assert timeout > 0

    def close(self, timeout):
        self.closed = True


def arguments(**overrides):
    fields = dict(endpoint="wss://deerflow.example.com/v1", protocol=10, timeout=1,
                  reconnect_cycles=2, max_pages=8, inspect_session=True,
                  session_id=None, abrupt_first_disconnect=True)
    fields.update(overrides)
    return SimpleNamespace(**fields)


class ReadOnlyWssProbeTest(unittest.TestCase):
    def test_reconnect_only_observes_existing_session_and_does_not_log_private_values(self):
        sockets = []
        def factory(endpoint, timeout):
            connection = FakeSocket()
            sockets.append(connection)
            return connection
        report = probe.verify(arguments(), TOKEN, factory)
        self.assertTrue(report["ok"])
        self.assertTrue(report["reconnectVerified"])
        self.assertEqual(len(sockets), 2)
        self.assertEqual(report["cycles"][0]["replayEvents"], {"skipped": "no_live_runtime"})
        for connection in sockets:
            self.assertTrue(connection.closed)
            self.assertEqual([wire["command"]["type"] for wire in connection.sent[1:]],
                             ["getSettings", "loadTaskState", "hydrateSession", "attachSession"])
        self.assertNotIn(TOKEN, json.dumps(report))
        self.assertNotIn("private conversation content", json.dumps(report))
        self.assertNotIn(SESSION, json.dumps(report))

    def test_live_replay_uses_fixed_boundary_and_allows_ephemeral_sequence_gaps(self):
        connection = FakeSocket(runtime=RUNTIME)
        report = probe.verify(arguments(reconnect_cycles=1), TOKEN, lambda *_: connection)
        self.assertTrue(report["ok"])
        self.assertEqual(report["cycles"][0]["replayEvents"], {"ok": True, "complete": True, "pages": 2, "events": 2})
        calls = [wire["command"] for wire in connection.sent if wire.get("command", {}).get("type") == "replayEvents"]
        self.assertIsNone(calls[0]["throughSequence"])
        self.assertEqual(calls[1]["throughSequence"], 3)
        self.assertEqual(calls[1]["epoch"], EPOCH)

    def test_protocol_mismatch_exposes_only_controlled_numeric_versions(self):
        connection = FakeSocket(hello={"type": "rejected", "message": "protocol 10 is unsupported; expected 9"})
        report = probe.verify(arguments(), TOKEN, lambda *_: connection)
        self.assertFalse(report["ok"])
        self.assertEqual(report["failure"], {"stage": "hello", "code": "protocol_mismatch", "requested": 10, "expected": 9})
        self.assertEqual(len(connection.sent), 1)

    def test_rpc_errors_never_echo_server_error_body(self):
        connection = FakeSocket(fail_rpc=True)
        report = probe.verify(arguments(), TOKEN, lambda *_: connection)
        self.assertFalse(report["ok"])
        self.assertEqual(report["failure"]["code"], "read_only_rpc_rejected")
        self.assertNotIn(TOKEN, json.dumps(report))
        self.assertNotIn("private conversation content", json.dumps(report))

    def test_mutating_commands_are_blocked_before_send(self):
        client = probe.WakuProbe("wss://example.com/v1", TOKEN)
        for kind in ["start", "prompt", "saveTaskState", "cancel", "respond", "removeSession", "deerFlow"]:
            with self.subTest(kind=kind), self.assertRaisesRegex(probe.VerificationError, "mutation_command_blocked"):
                client.rpc({"type": kind})

    def test_replay_rejects_changed_boundary_and_other_session(self):
        with self.assertRaisesRegex(probe.VerificationError, "replay_boundary_changed"):
            probe.validate_replay_page(page([event(3)], 4, 4, False), SESSION, RUNTIME, 1, EPOCH, 3)
        other = event(3)
        other["sessionId"] = RUNTIME
        with self.assertRaisesRegex(probe.VerificationError, "replay_identity_mismatch"):
            probe.validate_replay_page(page([other], 3, 3, False), SESSION, RUNTIME, 1, EPOCH, 3)

    def test_replay_page_cap_is_explicit(self):
        connection = FakeSocket(runtime=RUNTIME)
        report = probe.verify(arguments(reconnect_cycles=1, max_pages=1), TOKEN, lambda *_: connection)
        self.assertTrue(report["ok"])
        replay = report["cycles"][0]["replayEvents"]
        self.assertFalse(replay["complete"])
        self.assertTrue(replay["bounded"])

    def test_endpoint_rejects_embedded_token_and_path_rewrite(self):
        for endpoint in ["wss://user:secret@example.com/v1", "wss://example.com/v1?token=secret", "wss://example.com/api/v1"]:
            with self.subTest(endpoint=endpoint), self.assertRaises(probe.VerificationError):
                probe.validate_endpoint(endpoint)


if __name__ == "__main__":
    unittest.main()
