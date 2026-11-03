"""A sample Python app: GET / answers with message.txt."""
import http.server
import os
import platform
import socketserver
import sys


class Server(http.server.ThreadingHTTPServer):
    def server_bind(self):
        # http.server looks up the name of the address it binds, and a
        # reverse lookup of 127.0.0.1 can take half a minute on some hosts
        # (macOS among them): bind without it.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address[:2]


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
    server = Server(("127.0.0.1", int(os.environ["PORT"])), Handler)
    print(f"listening on 127.0.0.1:{server.server_port}")
    server.serve_forever()
