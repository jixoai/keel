import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
BODIES = sys.argv[2]
REPLY = "KITE-CLI-STUB-REPLY 已收到。"

def record(doc):
    with open(BODIES, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")

def chunks(model):
    def wrap(delta, finish=None, usage=None):
        out = {
            "id": "chatcmpl-kite-cli-stub", "object": "chat.completion.chunk",
            "created": 1735689600, "model": model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
        }
        if usage is not None:
            out["usage"] = usage
        return out
    yield wrap({"role": "assistant"})
    for start in range(0, len(REPLY), 6):
        yield wrap({"content": REPLY[start:start + 6]})
    yield wrap({}, finish="stop",
               usage={"prompt_tokens": 0, "completion_tokens": len(REPLY), "total_tokens": len(REPLY)})

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
        except Exception:
            doc = {}
        text = " ".join(
            str(m.get("content") or "") for m in (doc.get("messages") or []) if m.get("role") == "user"
        )
        record({
            "path": self.path,
            "model": doc.get("model"),
            "messages": doc.get("messages"),
            "tools": [t.get("function", {}).get("name") for t in (doc.get("tools") or [])],
            "slow": "SLOW" in text,
        })
        if "SLOW" in text:
            # Hold the turn open so the E2E can stop a genuinely running run.
            time.sleep(8)
        body = "".join("data: %s\n\n" % json.dumps(c, ensure_ascii=False) for c in chunks(doc.get("model") or "stub"))
        body += "data: [DONE]\n\n"
        encoded = body.encode("utf-8")
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(encoded)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(encoded)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, *args):
        pass

ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
