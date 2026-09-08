from __future__ import annotations

import json
import sys
import uuid

running_sessions: set[str] = set()


def send(message: dict) -> None:
    print(json.dumps(message, separators=(",", ":")), flush=True)


def respond(request: dict, result: dict) -> None:
    send({"jsonrpc": "2.0", "id": request["id"], "result": result})


def update(session_id: str, value: dict) -> None:
    send(
        {
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {"sessionId": session_id, "update": value},
        }
    )


for line in sys.stdin:
    request = json.loads(line)
    method = request.get("method")
    params = request.get("params", {})
    if method == "initialize":
        respond(
            request,
            {
                "protocolVersion": 2,
                "info": {"name": "fake-deerflow-v2", "version": "1"},
                "capabilities": {"session": {}},
            },
        )
    elif method == "session/new":
        session_id = f"session-{uuid.uuid4()}"
        respond(request, {"sessionId": session_id})
        update(session_id, {"sessionUpdate": "state_update", "state": "idle"})
    elif method == "session/resume":
        respond(request, {})
        update(
            params["sessionId"],
            {"sessionUpdate": "state_update", "state": "idle"},
        )
    elif method == "session/close":
        respond(request, {})
    elif method == "session/prompt":
        session_id = params["sessionId"]
        text = "".join(
            block.get("text", "")
            for block in params.get("prompt", [])
            if block.get("type") == "text"
        )
        respond(request, {})
        update(
            session_id,
            {"sessionUpdate": "state_update", "state": "running"},
        )
        update(
            session_id,
            {
                "sessionUpdate": "tool_call",
                "toolCallId": "tool-1",
                "title": "test_tool",
                "kind": "other",
                "status": "pending",
                "rawInput": {"secret": "test-only"},
            },
        )
        update(
            session_id,
            {
                "sessionUpdate": "tool_call_update",
                "toolCallId": "tool-1",
                "status": "completed",
                "content": [{"type": "text", "text": "test result"}],
            },
        )
        running_sessions.add(session_id)
        if text == "hang":
            continue
        if text == "fail":
            update(
                session_id,
                {
                    "sessionUpdate": "state_update",
                    "state": "idle",
                    "stopReason": "_deerflow_error",
                    "_meta": {"deerflowError": "fake failure"},
                },
            )
            running_sessions.discard(session_id)
            continue
        if text == "permission":
            send(
                {
                    "jsonrpc": "2.0",
                    "id": "permission-1",
                    "method": "session/request_permission",
                    "params": {
                        "sessionId": session_id,
                        "toolCall": {"toolCallId": "tool-1", "title": "Test"},
                        "options": [
                            {
                                "optionId": "allow-once",
                                "name": "Allow once",
                                "kind": "allow_once",
                            }
                        ],
                    },
                }
            )
            permission_response = json.loads(sys.stdin.readline())
            selected = permission_response.get("result", {}).get("outcome", {})
            if selected.get("optionId") != "allow-once":
                update(
                    session_id,
                    {
                        "sessionUpdate": "state_update",
                        "state": "idle",
                        "stopReason": "_deerflow_error",
                        "_meta": {"deerflowError": "permission was not approved"},
                    },
                )
                running_sessions.discard(session_id)
                continue
        update(
            session_id,
            {
                "sessionUpdate": "agent_message_chunk",
                "messageId": "answer-1",
                "content": {"type": "text", "text": "discarded draft"},
            },
        )
        update(
            session_id,
            {
                "sessionUpdate": "agent_message",
                "messageId": "answer-1",
                "content": [
                    {"type": "text", "text": f"Fake DeerFlow received: {text}"}
                ],
            },
        )
        update(
            session_id,
            {
                "sessionUpdate": "state_update",
                "state": "idle",
                "stopReason": "end_turn",
            },
        )
        running_sessions.discard(session_id)
    elif method == "session/cancel":
        session_id = params["sessionId"]
        if session_id in running_sessions:
            update(
                session_id,
                {
                    "sessionUpdate": "state_update",
                    "state": "idle",
                    "stopReason": "cancelled",
                },
            )
            running_sessions.discard(session_id)
    elif "id" in request:
        send(
            {
                "jsonrpc": "2.0",
                "id": request["id"],
                "error": {"code": -32601, "message": f"unknown method {method}"},
            }
        )
