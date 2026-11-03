#!/usr/bin/env python3
"""A sample app run as it is, with the host's Python."""
import http.server
import os
import platform


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body = "ok\n"
        else:
            with open("message.txt") as f:
                body = f"{f.read().strip()} from a binary ({platform.python_implementation()})\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(body.encode())


server = http.server.ThreadingHTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler)
print(f"listening on 127.0.0.1:{server.server_port}", flush=True)
server.serve_forever()
