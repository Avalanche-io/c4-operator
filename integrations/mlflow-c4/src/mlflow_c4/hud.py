"""A small client of the C4 HUD API (internal/hud/API.md in c4-operator),
standard library only.

Configuration, in order: explicit arguments, then the environment:

- ``C4_HUD``: the HUD's base URL, for example ``http://c4-hud.c4-system``;
- ``C4_HUD_TOKEN`` or ``C4_HUD_TOKEN_FILE``: the caller's bearer token
  (the file is read on every request, so a rotated token takes effect);
- ``C4_PROJECT``: the C4 project whose store holds the views.
"""

import json
import os
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Optional


class HUDError(Exception):
    """A refusal from the HUD, with its status and message."""

    def __init__(self, status: int, message: str):
        super().__init__(f"C4 HUD {status}: {message}")
        self.status = status
        self.message = message


class HUD:
    def __init__(self, base: Optional[str] = None, token: Optional[str] = None,
                 token_file: Optional[str] = None, agent: str = "mlflow-c4", timeout: float = 60):
        self.base = (base or os.environ.get("C4_HUD") or "").rstrip("/")
        if not self.base:
            raise ValueError("the C4 HUD's URL is required: backend config 'hud', or C4_HUD")
        self._token = token if token is not None else os.environ.get("C4_HUD_TOKEN")
        self._token_file = token_file or os.environ.get("C4_HUD_TOKEN_FILE")
        self.agent = agent
        self.timeout = timeout

    def _bearer(self) -> str:
        if self._token_file:
            with open(self._token_file, encoding="utf-8") as f:
                return f.read().strip()
        return self._token or ""

    def request(self, method: str, path: str, body: Any = None, raw: Optional[bytes] = None,
                query: Optional[dict] = None, ctype: str = "application/json") -> bytes:
        url = self.base + "/api/v1" + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        data = raw
        headers = {"Accept": "application/json", "X-C4-Client": "mlflow-c4", "X-C4-Agent": self.agent}
        if body is not None:
            data = json.dumps(body).encode()
        if data is not None:
            headers["Content-Type"] = ctype
        tok = self._bearer()
        if tok:
            headers["Authorization"] = "Bearer " + tok
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            msg = e.read().decode(errors="replace")
            try:
                msg = json.loads(msg).get("error", msg)
            except ValueError:
                pass
            raise HUDError(e.code, msg) from None

    def json(self, method: str, path: str, body: Any = None, query: Optional[dict] = None) -> Any:
        out = self.request(method, path, body=body, query=query)
        return json.loads(out) if out else None

    # --- the calls the plugins make ---

    def missing(self, project: str, ids: list) -> list:
        return self.json("POST", f"/objects/{q(project)}/missing", {"ids": ids})["missing"]

    def put_object(self, project: str, c4id: str, data: bytes) -> None:
        self.request("PUT", f"/objects/{q(project)}/{c4id}", raw=data, ctype="application/octet-stream")

    def post_view(self, project: str, files: list) -> str:
        return self.json("POST", f"/views/{q(project)}", {"files": files})["view"]

    def view_files(self, project: str, view: str, path: str = "") -> list:
        query = {"limit": "100000"}
        if path:
            query["path"] = path
        return self.json("GET", f"/views/{q(project)}/{view}/files", query=query)["files"]

    def get_object(self, project: str, c4id: str) -> bytes:
        return self.request("GET", f"/objects/{q(project)}/{c4id}")

    def submit(self, request: dict) -> dict:
        return self.json("POST", "/jobs", request)

    def job(self, namespace: str, name: str) -> dict:
        return self.json("GET", f"/jobs/{q(namespace)}/{q(name)}")

    def nudge(self, namespace: str, name: str, body: dict) -> dict:
        return self.json("POST", f"/jobs/{q(namespace)}/{q(name)}/nudge", body)


def q(s: str) -> str:
    return urllib.parse.quote(s, safe="")
