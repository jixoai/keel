# The recording stub: captures every /v1/chat/completions request body and
# answers with a valid OpenAI streaming response. The slow models are
# delayed before the response starts, giving the driver a window in which the
# attempt is demonstrably in flight (the request is already recorded).
import json, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
BODIES = sys.argv[2]
LOG = sys.argv[3]
SLOW_SECONDS = float(sys.argv[4])

REPLY = "KITE-START-STUB-REPLY 已收到。"

def record(doc):
    with open(BODIES, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")

def chunks(model):
    def wrap(delta, finish=None, usage=None):
        out = {
            "id": "chatcmpl-kite-start-stub",
            "object": "chat.completion.chunk",
            "created": 1735689600,
            "model": model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
        }
        if usage is not None:
            out["usage"] = usage
        return out
    yield wrap({"role": "assistant"})
    for start in range(0, len(REPLY), 6):
        yield wrap({"content": REPLY[start:start + 6]})
    yield wrap({}, finish="stop", usage={"prompt_tokens": 0, "completion_tokens": len(REPLY), "total_tokens": len(REPLY)})

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
        except Exception:
            doc = {}
        auth = self.headers.get("Authorization") or ""
        model = doc.get("model") or "stub"
        tools = [t.get("function", {}).get("name") for t in (doc.get("tools") or [])]
        record({
            "path": self.path,
            "model": model,
            "stream": doc.get("stream"),
            "messages": doc.get("messages"),
            "tools": tools,
            "authPresent": bool(auth),
        })
        with open(LOG, "a", encoding="utf-8") as fh:
            fh.write("POST %s model=%s tools=%s\n" % (self.path, model, tools))
        if model in ("kite-slow", "kite-team-slow"):
            time.sleep(SLOW_SECONDS)
        body = "".join("data: %s\n\n" % json.dumps(c, ensure_ascii=False) for c in chunks(model))
        body += "data: [DONE]\n\n"
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
