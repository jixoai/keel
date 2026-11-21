import json, os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
LOG = sys.argv[2]
EXPECTED = os.environ.get("STUB_EXPECTED_KEY", "")

LIVE_MODELS = ["stub-model-a", "stub-model-b", "stub-model-live"]

def record(doc):
    with open(LOG, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(doc, ensure_ascii=False) + "\n")

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        authorized = self.headers.get("Authorization") == "Bearer " + EXPECTED
        record({"method": "GET", "path": self.path, "authorized": authorized})
        if self.path.rstrip("/") != "/v1/models":
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if not authorized:
            self.send_response(401)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        body = json.dumps({"data": [{"id": m} for m in LIVE_MODELS]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
