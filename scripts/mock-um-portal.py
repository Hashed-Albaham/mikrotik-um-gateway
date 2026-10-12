#!/usr/bin/env python3
"""Local development stub for the RouterOS v7 User Manager web portal.

It implements the portal endpoints the gateway talks to, in the documented
RouterOS v7 shape, so the service can be exercised without a router. Set
PORTAL_SHAPE=legacy to answer with the older, flatter shape instead and check
that the tolerant parsing still resolves the same numbers.

This file is a development aid only; it is not part of the container image.

Usage:
    python3 scripts/mock-um-portal.py [port] [user] [password]
    PORTAL_SHAPE=legacy python3 scripts/mock-um-portal.py 8099
"""

from __future__ import annotations

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8099
USER = sys.argv[2] if len(sys.argv) > 2 else "0018067158"
PASSWORD = sys.argv[3] if len(sys.argv) > 3 else "demo"
PROFILE = os.environ.get("PORTAL_PROFILE", "1000-1G-8h")
SHAPE = os.environ.get("PORTAL_SHAPE", "v7")
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

    def _read_form(self) -> dict:
        length = int(self.headers.get("Content-Length") or 0)
        if length == 0:
            return {}
        raw = self.rfile.read(length).decode("utf-8", "replace")
        return {key: values[0] for key, values in parse_qs(raw).items()}

    def do_POST(self) -> None:  # noqa: N802 - required by BaseHTTPRequestHandler
        if self.path.startswith("/um/api/login"):
            form = self._read_form()
            if form.get("username") == USER and form.get("password") == PASSWORD:
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
        # Longer paths first: "/um/api/getUser" prefixes "/um/api/getUserProfiles".
        if self.path.startswith("/um/api/getUserProfiles"):
            if SHAPE == "legacy":
                self._send(200, [{"user": USER, "profile": PROFILE, "state": "running active"}])
            else:
                self._send(
                    200,
                    {
                        "success": True,
                        "data": {
                            "profiles": [
                                {
                                    "id": 1,
                                    "name": PROFILE,
                                    "profileId": 5,
                                    "state": 1,
                                    "expAfter": "6h 30m",
                                }
                            ]
                        },
                    },
                )
            return
        if self.path.startswith("/um/api/getUser"):
            if SHAPE == "legacy":
                self._send(
                    200,
                    [
                        {
                            "name": USER,
                            "download-used": int(0.75 * GIB),
                            "upload-used": int(0.05 * GIB),
                            "uptime": 5400,
                            "active-sessions": 1,
                        }
                    ],
                )
            else:
                self._send(
                    200,
                    {
                        "success": True,
                        "data": {
                            "username": USER,
                            "download": f"{int(0.75 * GIB / MIB)}M",
                            "upload": f"{int(0.05 * GIB / MIB)}M",
                            "uptime": "1h30m",
                            "activeSess": 1,
                            "sharedUsers": 1,
                        },
                    },
                )
            return
        self._send(404, {"error": "not found"})


if __name__ == "__main__":
    print(
        f"mock user manager portal on http://127.0.0.1:{PORT} "
        f"(user={USER}, profile={PROFILE}, shape={SHAPE})",
        flush=True,
    )
    ThreadingHTTPServer(("127.0.0.1", PORT), PortalHandler).serve_forever()
