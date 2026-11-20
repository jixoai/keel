#!/usr/bin/env python3
"""TCP trap listener for the P6A2a CSP scenario.

The browser under test is launched with

    --host-resolver-rules=MAP evil.fixture.test 127.0.0.1:<this port>

so any request the browser actually lets through to
`https://evil.fixture.test/leak` becomes a TCP connection here. A blocked
request (the expected case: the app document's `connect-src` does not declare
that host) produces no connection at all.

The log is JSONL: one line per accepted connection with the peer, the first
bytes (hex and printable prefix) and their length. It never sees a real
credential: the only client is the local test browser and the URL is the
fixture's fake host.

Usage: leak-listener.py PORT LOG
"""

import json
import socket
import sys
import threading
import time

PORT = int(sys.argv[1])
LOG = sys.argv[2]
lock = threading.Lock()


def record(doc):
    with lock, open(LOG, "a", encoding="utf-8") as handle:
        handle.write(json.dumps(doc, ensure_ascii=False) + "\n")


def serve(conn, peer):
    conn.settimeout(2.0)
    payload = b""
    try:
        payload = conn.recv(4096)
    except OSError:
        pass
    finally:
        try:
            conn.close()
        except OSError:
            pass
    printable = "".join(chr(byte) if 32 <= byte < 127 else "." for byte in payload[:16])
    record(
        {
            "at": time.time(),
            "peer": list(peer),
            "bytes": len(payload),
            "firstHex": payload[:16].hex(),
            "printable": printable,
            # A TLS ClientHello starts with 0x16 (handshake), an HTTP request
            # with an ASCII method: enough to name what the browser attempted.
            "firstByte": payload[0] if payload else None,
        }
    )


def main():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(("127.0.0.1", PORT))
    server.listen(16)
    record({"at": time.time(), "event": "listening", "port": PORT})
    while True:
        try:
            conn, peer = server.accept()
        except OSError:
            break
        threading.Thread(target=serve, args=(conn, peer), daemon=True).start()


if __name__ == "__main__":
    main()
