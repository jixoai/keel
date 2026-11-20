#!/usr/bin/env python3
"""Recording OpenAI-compatible stub for the MCP Apps browser E2E.

The model streams one aggregate `mcp` tool call per request until every tool in
MCP_E2E_TOOL_SEQUENCE has been called; the request that follows the last tool
result streams a short final answer. The default sequence is
`render_weather`, which is exactly the P6A1 behavior:

    {"action":"call","server":"e2e","tool":"render_weather",
     "arguments":{"city":"Shanghai"}}

That is the shape internal/toolsurface/mcp.go expects. P6A2a sets
`MCP_E2E_TOOL_SEQUENCE=render_weather,render_broken` (handshake degradation) or
`MCP_E2E_TOOL_SEQUENCE=render_leaky` (CSP degradation).

The log is a compact JSONL summary only: model, tool names, whether the request
carried a tool result. No message bodies, no Authorization value.
"""

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
LOG = sys.argv[2]

TOOL_SEQUENCE = [
    name.strip()
    for name in (os.environ.get("MCP_E2E_TOOL_SEQUENCE") or "render_weather").split(",")
    if name.strip()
]
FINAL_TEXT = "已完成天气面板渲染。"


def tool_call_arguments(name):
    return json.dumps(
        {
            "action": "call",
            "server": "e2e",
            "tool": name,
            "arguments": {"city": "Shanghai"},
        },
        ensure_ascii=False,
    )


def record(doc):
    with open(LOG, "a", encoding="utf-8") as handle:
        handle.write(json.dumps(doc, ensure_ascii=False) + "\n")


def chunk(model, delta, finish=None, usage=None):
    out = {
        "id": "chatcmpl-mcp-apps-e2e",
        "object": "chat.completion.chunk",
        "created": 1735689600,
        "model": model,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }
    if usage is not None:
        out["usage"] = usage
    return out


def stream_body(model, tool_messages):
    events = []
    if tool_messages < len(TOOL_SEQUENCE):
        name = TOOL_SEQUENCE[tool_messages]
        events.append(chunk(model, {"role": "assistant"}))
        events.append(
            chunk(
                model,
                {
                    "tool_calls": [
                        {
                            "index": 0,
                            "id": "call_mcp_apps_e2e_%d" % (tool_messages + 1),
                            "type": "function",
                            "function": {"name": "mcp", "arguments": tool_call_arguments(name)},
                        }
                    ]
                },
            )
        )
        events.append(chunk(model, {}, finish="tool_calls", usage={"prompt_tokens": 0, "completion_tokens": 1, "total_tokens": 1}))
    else:
        events.append(chunk(model, {"role": "assistant"}))
        for start in range(0, len(FINAL_TEXT), 4):
            events.append(chunk(model, {"content": FINAL_TEXT[start : start + 4]}))
        events.append(chunk(model, {}, finish="stop", usage={"prompt_tokens": 0, "completion_tokens": len(FINAL_TEXT), "total_tokens": len(FINAL_TEXT)}))
    return "".join("data: %s\n\n" % json.dumps(event, ensure_ascii=False) for event in events) + "data: [DONE]\n\n"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        if self.path.rstrip("/") == "/v1/models":
            body = json.dumps(
                {
                    "object": "list",
                    "data": [
                        {"id": "kite-e2e-model", "object": "model", "owned_by": "stub"},
                        {"id": "kite-member-model", "object": "model", "owned_by": "stub"},
                    ],
                }
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
        except Exception:
            doc = {}
        messages = doc.get("messages") or []
        tool_messages = sum(1 for message in messages if message.get("role") == "tool")
        has_tool_result = tool_messages > 0
        tools = [tool.get("function", {}).get("name") for tool in (doc.get("tools") or [])]
        record(
            {
                "kind": "request",
                "path": self.path,
                "model": doc.get("model"),
                "tools": tools,
                "hasToolResult": has_tool_result,
                "toolMessages": tool_messages,
                "roles": [message.get("role") for message in messages],
                "authPresent": bool(self.headers.get("Authorization")),
            }
        )
        body = stream_body(doc.get("model") or "kite-e2e-model", tool_messages)
        encoded = body.encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
