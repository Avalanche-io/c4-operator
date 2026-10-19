"""A fake C4 HUD for the plugins' tests: the routes they call, in the
shapes internal/hud/API.md gives (Submitting; GET /jobs; nudge)."""

import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import pytest

from mlflow_c4.c4id import identify

TOKEN = "t0k"


class FakeHUD:
    def __init__(self):
        self.objects = {}  # id -> bytes
        self.views = {}  # view -> [files]
        self.jobs = []  # submit requests
        self.phases = {}  # name -> phase
        self.nudges = []
        self.puts = 0
        self.lock = threading.Lock()

    def add_view(self, files: dict) -> str:
        """Stores {path: bytes} as a view, returns its (fake) ID."""
        listing = []
        for p, data in sorted(files.items()):
            cid = identify(data)
            self.objects[cid] = data
            listing.append({"path": p, "id": cid, "size": len(data)})
        view = identify(json.dumps(listing, sort_keys=True).encode())
        self.views[view] = listing
        return view


def handler(hud: FakeHUD):
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _send(self, code, body=None, raw=None):
            data = raw if raw is not None else json.dumps(body or {}).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/octet-stream" if raw is not None else "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _auth(self):
            if self.headers.get("Authorization") != "Bearer " + TOKEN:
                self._send(401, {"error": "unauthorized"})
                return False
            return True

        def _body(self):
            n = int(self.headers.get("Content-Length") or 0)
            return self.rfile.read(n) if n else b""

        def do_GET(self):
            if not self._auth():
                return
            u = urlparse(self.path)
            q = parse_qs(u.query)
            if m := re.fullmatch(r"/api/v1/views/([^/]+)/([^/]+)/files", u.path):
                files = hud.views.get(m.group(2))
                if files is None:
                    return self._send(404, {"error": "not found"})
                pre = q.get("path", [""])[0].strip("/")
                out = [f for f in files if not pre or f["path"] == pre or f["path"].startswith(pre + "/")]
                return self._send(200, {"view": m.group(2), "files": out})
            if m := re.fullmatch(r"/api/v1/objects/([^/]+)/([^/]+)", u.path):
                data = hud.objects.get(m.group(2))
                return self._send(404, {"error": "not found"}) if data is None else self._send(200, raw=data)
            if m := re.fullmatch(r"/api/v1/jobs/([^/]+)/([^/]+)", u.path):
                ph = hud.phases.get(m.group(2))
                if ph is None:
                    return self._send(404, {"error": "not found"})
                return self._send(200, {"namespace": m.group(1), "name": m.group(2), "phase": ph})
            self._send(404, {"error": "no route " + u.path})

        def do_PUT(self):
            if not self._auth():
                return
            u = urlparse(self.path)
            if m := re.fullmatch(r"/api/v1/objects/([^/]+)/([^/]+)", u.path):
                data = self._body()
                if identify(data) != m.group(2):
                    return self._send(400, {"error": "the bytes are not " + m.group(2)})
                with hud.lock:
                    stored = m.group(2) not in hud.objects
                    hud.objects[m.group(2)] = data
                    hud.puts += 1
                return self._send(200, {"ok": True, "id": m.group(2), "stored": stored})
            self._send(404, {"error": "no route"})

        def do_POST(self):
            if not self._auth():
                return
            u = urlparse(self.path)
            body = json.loads(self._body() or b"{}")
            if re.fullmatch(r"/api/v1/objects/([^/]+)/missing", u.path):
                return self._send(200, {"missing": [i for i in body["ids"] if i not in hud.objects]})
            if re.fullmatch(r"/api/v1/views/([^/]+)", u.path):
                missing = [f["id"] for f in body["files"] if f["id"] not in hud.objects]
                if missing:
                    return self._send(409, {"missing": missing})
                view = identify(json.dumps(sorted(body["files"], key=lambda f: f["path"]), sort_keys=True).encode())
                hud.views[view] = body["files"]
                return self._send(200, {"view": view, "files": len(body["files"])})
            if u.path == "/api/v1/jobs":
                with hud.lock:
                    hud.jobs.append(body)
                    ns = body.get("namespace") or body.get("project", "p") + "-work"
                    name = body.get("name") or f"{body.get('stage', 'job')}-v{len(hud.jobs):03d}"
                    hud.phases[name] = "Queued"
                return self._send(201, {"ok": True, "namespace": ns, "name": name, "project": body.get("project", "")})
            if m := re.fullmatch(r"/api/v1/jobs/([^/]+)/([^/]+)/nudge", u.path):
                hud.nudges.append((m.group(2), body))
                return self._send(200, {"ok": True, "message": "held"})
            self._send(404, {"error": "no route " + u.path})

    return H


@pytest.fixture
def hud(monkeypatch):
    h = FakeHUD()
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler(h))
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    h.url = f"http://127.0.0.1:{srv.server_port}"
    monkeypatch.setenv("C4_HUD", h.url)
    monkeypatch.setenv("C4_HUD_TOKEN", TOKEN)
    monkeypatch.setenv("C4_PROJECT", "saltmarsh")
    monkeypatch.delenv("C4_HUD_TOKEN_FILE", raising=False)
    yield h
    srv.shutdown()
    srv.server_close()
