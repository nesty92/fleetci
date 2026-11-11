"""Receives the matrix jobs' lines: each POST's body is one line, appended
to the file after the time it came. Jobs on GitHub and GitLab post when
they start and when they end, so realmatrix.sh needs no forge API to
follow them.

    python3 -I sink.py <file> [port]
"""
import http.server
import sys
import threading
import time

out, port = sys.argv[1], int(sys.argv[2]) if len(sys.argv) > 2 else 8099
lock = threading.Lock()


class Sink(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = min(int(self.headers.get("Content-Length") or 0), 64 << 10)
        line = self.rfile.read(n).decode("utf-8", "replace").replace("\n", " ").strip()
        with lock, open(out, "a") as f:
            f.write(f"{time.time():.1f} {line}\n")
        self.send_response(204)
        self.end_headers()

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("", port), Sink).serve_forever()
