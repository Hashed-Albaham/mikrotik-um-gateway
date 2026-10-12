#!/usr/bin/env python3
"""Local development stub for the RouterOS v7 User Manager web portal.

It implements the five portal endpoints the gateway talks to so the service can
be exercised without a router. This file is a development aid only; it is not
part of the container image.

Usage:
    python3 scripts/mock-um-portal.py [port] [user] [password]
"""

from __future__ import annotations

import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8099
USER = sys.argv[2] if len(sys.argv) > 2 else "0018067158"
PASSWORD = sys.argv[3] if len(sys.argv) > 3 else "demo"
PROFILE = "1000-1G-8h"
GIB = 1024**3
MIB = 1024**2


class PortalHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args: object) -> None:  # keep the sample output quiet
        pass

    def _send(self, status: int, payload: object, cookie: str | None = None) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if cookie:
            self.send_header("Set-Cookie", cookie)
        self.end_headers()
        self.wfile.write(body)

    def _authed(self) -> bool:
        return "um_session=" in (self.headers.get("Cookie") or "")

    def _read_body(self) -> dict:
        length = int(self.headers.get("Content-Length") or 0)
        if length == 0:
            return {}
        try:
            return json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            return {}

    def do_POST(self) -> None:  # noqa: N802 - required by BaseHTTPRequestHandler
        if self.path.startswith("/um/api/login"):
            body = self._read_body()
            if body.get("username") == USER and body.get("password") == PASSWORD:
                self._send(200, {"success": True}, cookie="um_session=demo; Path=/")
            else:
                self._send(200, {"success": False, "error": "invalid credentials"})
            return
        if self.path.startswith("/um/api/logout"):
            self._send(200, {"success": True})
            return
        self._send(404, {"error": "not found"})

    def do_GET(self) -> None:  # noqa: N802 - required by BaseHTTPRequestHandler
        if not self._authed():
            self._send(401, {"error": "no session"})
            return
        # The longer paths are matched first: "/um/api/getUser" is a prefix of
        # "/um/api/getUserProfiles".
        if self.path.startswith("/um/api/getUserProfiles"):
            self._send(
                200,
                [{"user": USER, "profile": PROFILE, "state": "running active"}],
            )
            return
        if self.path.startswith("/um/api/getUser"):
            self._send(
                200,
                [
                    {
                        "name": USER,
                        "actual-profile": PROFILE,
                        "download-used": int(0.75 * GIB),
                        "upload-used": int(0.05 * GIB),
                        "uptime": 5400,
                        "active-sessions": 1,
                        "state": "running active",
                    }
                ],
            )
            return
        if self.path.startswith("/um/api/getProfiles"):
            self._send(200, [{"name": PROFILE, "validity": "8h", "price": 1000}])
            return
        self._send(404, {"error": "not found"})


if __name__ == "__main__":
    print(f"mock user manager portal on http://127.0.0.1:{PORT} (user={USER})", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), PortalHandler).serve_forever()
