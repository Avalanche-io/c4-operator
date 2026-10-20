"""The farm, as a submitter reaches it: the C4 HUD API (c4-operator
``internal/hud/API.md``), as the artist's own identity, standard library
only.

Configuration, in order: explicit arguments, then the environment:

- ``C4_HUD``: the HUD's base URL, for example ``https://c4.petrelfinch.example``;
- ``C4_HUD_TOKEN`` or ``C4_HUD_TOKEN_FILE``: the artist's bearer token (the
  file is read on every request, so a rotated token takes effect).

The tools routes are the farm's (``GET /tools/{project}`` and
``/tools/{project}/{tool}``). Submissions and projects are not built yet;
their shapes here are the ones proposed for them on 2026-10-20, and change
with what lands.
"""

import json
import os
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional

from c4submit import __version__

MISSING_BATCH = 100_000   # POST /objects/{project}/missing takes at most this many


class FarmError(Exception):
    """A refusal or failure from the farm, with its status and message, and
    the answer's JSON body when it had one."""

    def __init__(self, status: int, message: str, body: Any = None):
        super().__init__(f"C4 farm {status}: {message}" if status else f"C4 farm: {message}")
        self.status = status
        self.message = message
        self.body = body


class NotOnFarm(FarmError):
    """The farm has no such route yet (404 or 501 for a route of slice 1)."""


def q(s: str) -> str:
    return urllib.parse.quote(s, safe="")


class Farm:
    def __init__(self, base: Optional[str] = None, token: Optional[str] = None, token_file: Optional[str] = None,
                 client: str = "c4submit", timeout: float = 120):
        self.base = (base or os.environ.get("C4_HUD") or "").rstrip("/")
        if not self.base:
            raise FarmError(0, "the farm's address is not set: set C4_HUD to the HUD's URL")
        self._token = token if token is not None else os.environ.get("C4_HUD_TOKEN")
        self._token_file = token_file or os.environ.get("C4_HUD_TOKEN_FILE")
        self.client = client
        self.timeout = timeout

    def _bearer(self) -> str:
        if self._token_file:
            try:
                with open(self._token_file, encoding="utf-8") as f:
                    return f.read().strip()
            except OSError as e:
                raise FarmError(0, f"cannot read the token file {self._token_file}: {e.strerror or e}") from None
        return self._token or ""

    def request(self, method: str, path: str, body: Any = None, data: Any = None, length: Optional[int] = None,
                query: Optional[dict] = None, ctype: str = "application/json") -> bytes:
        url = self.base + "/api/v1" + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        headers = {"Accept": "application/json", "X-C4-Client": f"{self.client}/{__version__}"}
        if body is not None:
            data = json.dumps(body).encode()
        if data is not None:
            headers["Content-Type"] = ctype
            if length is not None:
                headers["Content-Length"] = str(length)
        tok = self._bearer()
        if tok:
            headers["Authorization"] = "Bearer " + tok
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            raw = e.read().decode(errors="replace")
            parsed: Any = None
            msg = raw.strip() or e.reason
            try:
                parsed = json.loads(raw)
                if isinstance(parsed, dict) and parsed.get("error"):
                    msg = parsed["error"]
            except ValueError:
                pass
            raise FarmError(e.code, str(msg), parsed) from None
        except urllib.error.URLError as e:
            raise FarmError(0, f"cannot reach {self.base}: {e.reason}") from None
        except OSError as e:  # a timeout, a reset connection
            raise FarmError(0, f"lost the connection to {self.base}: {e}") from None

    def json(self, method: str, path: str, body: Any = None, query: Optional[dict] = None) -> Any:
        out = self.request(method, path, body=body, query=query)
        try:
            return json.loads(out) if out else None
        except ValueError:
            raise FarmError(0, f"{self.base} answered {path} with something that is not the farm's JSON "
                               "(a sign-in page in front of it?)") from None

    def _field(self, out: Any, key: str, path: str) -> Any:
        if not isinstance(out, dict) or key not in out:
            raise FarmError(0, f"{self.base} answered {path} without {key!r}")
        return out[key]

    # --- objects and views ----------------------------------------------

    def missing(self, project: str, ids: List[str]) -> List[str]:
        """The IDs the project's store lacks, in the order given."""
        out: List[str] = []
        for i in range(0, len(ids), MISSING_BATCH):
            path = f"/objects/{q(project)}/missing"
            out += self._field(self.json("POST", path, {"ids": ids[i:i + MISSING_BATCH]}), "missing", path)
        return out

    def put_file(self, project: str, cid: str, path: str) -> dict:
        """Uploads one file's bytes, streamed from disk."""
        try:
            f = open(path, "rb")
            size = os.fstat(f.fileno()).st_size
        except OSError as e:
            raise FarmError(0, f"cannot read {path}: {e.strerror or e}") from None
        with f:
            out = self.request("PUT", f"/objects/{q(project)}/{cid}", data=f, length=size,
                               ctype="application/octet-stream")
        return json.loads(out) if out else {}

    def post_view(self, project: str, view: dict) -> str:
        """Records a tree of stored objects; returns its ID."""
        path = f"/views/{q(project)}"
        return self._field(self.json("POST", path, view), "view", path)

    # --- what a submitter reads -------------------------------------------

    def _slice1(self, path: str, what: str) -> Any:
        try:
            return self.json("GET", path)
        except FarmError as e:
            if e.status in (404, 501):
                raise NotOnFarm(e.status, f"this farm does not serve {what} yet", e.body) from None
            raise

    def tools(self, project: str) -> List[dict]:
        """The tools the project may use, at the versions in use:
        ``[{name, title, summary, kind, version, view, image, scope}]``."""
        out = self._slice1(f"/tools/{q(project)}", "tools")
        return out.get("tools", out) if isinstance(out, dict) else out

    def tool(self, project: str, name: str) -> dict:
        """One tool at the version in use, with ``params``, ``frames`` and
        ``needs`` as its ``tool.yaml`` writes them."""
        return self._slice1(f"/tools/{q(project)}/{q(name)}", f"the tool {name}")

    def project(self, project: str) -> Optional[dict]:
        """What a submitter needs of the project, ``{root, timeZone, dueTime,
        milestones}``, or None on a farm that does not say yet."""
        try:
            return self._slice1(f"/projects/{q(project)}", "projects")
        except NotOnFarm:
            return None

    # --- submissions ------------------------------------------------------

    def _submission(self, doc: dict, dry_run: bool, explain: bool) -> dict:
        body: Dict[str, Any] = {"submission": doc}
        if dry_run:
            body["dryRun"] = True
        if explain:
            body["explain"] = True
        try:
            return self.json("POST", "/submissions", body)
        except FarmError as e:
            # A refusal of the file is an answer, with its errors; anything
            # else is a failure to report.
            if isinstance(e.body, dict) and e.body.get("errors"):
                out = dict(e.body)
                out["ok"] = False
                return out
            if e.status in (404, 501):
                raise NotOnFarm(e.status, "this farm does not take submissions yet (POST /submissions)", e.body) from None
            raise

    def check(self, doc: dict, explain: bool = False) -> dict:
        """The farm's check of a submission: its errors, or what it would
        make and when it would be ready. Nothing is created."""
        return self._submission(doc, True, explain)

    def submit(self, doc: dict) -> dict:
        return self._submission(doc, False, False)

