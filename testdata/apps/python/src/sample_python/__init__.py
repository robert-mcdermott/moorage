"""A sample Python app: GET / answers with message.txt."""
import http.server
import os
import platform
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body = "ok\n"
        else:
            with open("message.txt") as f:
                message = f.read().strip()
            venv = "in its virtual environment" if sys.prefix != sys.base_prefix else "outside a virtual environment"
            body = f"{message} from python {platform.python_version()} ({venv})\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(body.encode())


def main():
    port = int(os.environ["PORT"])
    print(f"listening on 127.0.0.1:{port}")
    http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
