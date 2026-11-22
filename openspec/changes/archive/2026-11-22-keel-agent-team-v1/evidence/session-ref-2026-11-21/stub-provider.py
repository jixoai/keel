import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
REPLY = "SESSION-REF-STUB-REPLY"

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            doc = json.loads(raw)
        except Exception:
            doc = {}
        chunks = [
            {"id": "chatcmpl-session-ref", "object": "chat.completion.chunk",
             "created": 1735689600, "model": doc.get("model") or "stub",
             "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]},
            {"id": "chatcmpl-session-ref", "object": "chat.completion.chunk",
             "created": 1735689600, "model": doc.get("model") or "stub",
             "choices": [{"index": 0, "delta": {"content": REPLY}, "finish_reason": "stop"}],
             "usage": {"prompt_tokens": 0, "completion_tokens": 1, "total_tokens": 1}},
        ]
        body = "".join("data: %s\n\n" % json.dumps(c, ensure_ascii=False) for c in chunks) + "data: [DONE]\n\n"
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
