#!/usr/bin/env python3
"""Serve a synthetic HTTPS prerequisite; never use real business content."""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ssl

BODY = b"helmr-private-business-canary\n"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = BODY if self.path == "/probe" else b"not found\n"
        self.send_response(200 if self.path == "/probe" else 404)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    args = parser.parse_args()
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(args.cert, args.key)
    server = ThreadingHTTPServer(("0.0.0.0", 443), Handler)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
