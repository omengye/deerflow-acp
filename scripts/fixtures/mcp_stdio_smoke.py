"""Small stdio MCP server for the isolated desktop ACP smoke test."""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path


def reply(request_id: object, result: object) -> None:
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": request_id, "result": result}) + "\n")
    sys.stdout.flush()


def main() -> None:
    for line in sys.stdin:
        request = json.loads(line)
        request_id = request.get("id")
        method = request.get("method")
        if request_id is None:
            continue
        if method == "initialize":
            reply(request_id, {
                "protocolVersion": request.get("params", {}).get("protocolVersion"),
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "desktop-mcp-smoke", "version": "1"},
            })
        elif method == "tools/list":
            reply(request_id, {"tools": [{
                "name": "record",
                "description": "Record the isolated desktop smoke value",
                "inputSchema": {"type": "object", "properties": {"value": {"type": "string"}},
                                "required": ["value"]},
            }]})
        elif method == "tools/call":
            if request.get("params", {}).get("name") != "record" or not os.environ.get("ACP_MCP_SECRET"):
                reply(request_id, {"isError": True, "content": [{"type": "text", "text": "fixture rejected call"}]})
                continue
            value = request["params"]["arguments"]["value"]
            Path(os.environ["ACP_MCP_EFFECT"]).write_text(value, encoding="utf-8")
            reply(request_id, {"content": [{"type": "text", "text": "recorded successfully"}]})
        else:
            reply(request_id, {})


if __name__ == "__main__":
    main()
