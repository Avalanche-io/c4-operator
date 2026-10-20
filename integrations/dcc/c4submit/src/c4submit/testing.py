"""Fakes for testing c4submit and the DCC panels without a farm or a DCC.

- ``FakeFarm``: the farm in memory, with the same methods as ``Farm``. Its
  check answers the way the submission format's examples show, until
  ``POST /submissions`` is built and its answers can be recorded.
- ``serve(farm)``: the same farm over HTTP on localhost, so ``Farm`` itself
  (urllib, streaming uploads, refusals) is tested end to end.
- ``FakeHost``: a scene described by data.
"""

import json
import os
import threading
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, List, Optional
from urllib.parse import unquote, urlparse

from c4submit import frames as framelib
from c4submit import submission
from c4submit.c4id import identify
from c4submit.farm import FarmError, NotOnFarm
from c4submit.host import Dependency, Host, Renderable
from c4submit.tool import Tool
from c4submit.tree import Tree

NOW = datetime(2026, 10, 22, 21, 0, tzinfo=timezone.utc)

NUKE_COMP = {
    "c4": "tool/1", "name": "nuke-comp", "title": "Nuke comp",
    "summary": "Renders a Nuke script's Write node over frames.",
    "kind": "frames",
    "runs": {"image": "registry.petrelfinch.example/nuke:16.0", "exe": "nuke",
             "command": ["{{Run.Exe}}", "-x", "-X", "{{Param.write}}", "-F", "{{Unit.Frames}}", "{{Param.script}}"]},
    "frames": {"chunk": 4, "order": "binary"},
    "params": [
        {"name": "script", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
         "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "Script",
                           "fileFilters": [{"label": "Nuke", "patterns": ["*.nk"]}]}},
        {"name": "write", "type": "STRING", "default": "Write_main", "from": "scene.renderables",
         "userInterface": {"label": "Write node"}},
        {"name": "proxy", "type": "BOOL", "default": False, "show": "advanced",
         "userInterface": {"label": "Proxy mode"}},
        {"name": "threads", "type": "INT", "default": 0, "minValue": 0, "maxValue": 128, "show": "advanced",
         "affectsOutput": False},
    ],
    "needs": {"cpu": 4, "memory": "16G", "licences": {"foundry/nuke_r": 1}},
    "reuse": "exact",
}

KARMA = {
    "c4": "tool/1", "name": "karma", "title": "Karma render",
    "summary": "Renders a USD stage's render settings with Karma XPU or CPU to OpenEXR.",
    "kind": "frames",
    "runs": {"image": "registry.petrelfinch.example/houdini:21.0.440", "exe": "husk",
             "command": ["{{Run.Exe}}", "--renderer", "{{Param.renderer}}", "--settings", "{{Param.settings}}",
                         "-f", "{{Unit.FrameStart}}", "-n", "{{Unit.FrameCount}}", "-V", "2a", "{{Param.stage}}"]},
    "frames": {"chunk": 1, "order": "binary"},
    "params": [
        {"name": "stage", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
         "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "USD stage",
                           "fileFilters": [{"label": "USD", "patterns": ["*.usd", "*.usda", "*.usdc"]}]}},
        {"name": "settings", "type": "STRING", "default": "/Render/rendersettings", "from": "scene.renderables",
         "userInterface": {"label": "Render settings"}},
        {"name": "renderer", "type": "STRING", "default": "BRAY_HdKarmaXPU",
         "allowedValues": ["BRAY_HdKarmaXPU", "BRAY_HdKarma"],
         "userInterface": {"control": "DROPDOWN_LIST", "label": "Engine"}},
    ],
    "needs": {"cpu": 8, "memory": "64G", "gpu": 1, "gpuMemory": "24G", "licences": {"sesi/karma": 1},
              "sizes": {"heavy": {"memory": "128G", "gpuMemory": "48G"}}},
    "reuse": "exact",
}

TRANSCODE = {
    "c4": "tool/1", "name": "transcode", "title": "Transcode", "kind": "once",
    "runs": {"image": "c4/runtime-oiio", "command": ["c4-transcode"]},
    "params": [{"name": "codec", "type": "STRING", "default": "prores-422hq"}],
}

SALTMARSH = {
    "name": "saltmarsh", "root": "/show", "timeZone": "America/Los_Angeles", "dueTime": "18:00",
    "milestones": [
        {"name": "dailies", "label": "Dailies, Thu 18:00", "at": "2026-10-23T01:00:00Z"},
        {"name": "client-review", "label": "Client review, Fri 10:00", "at": "2026-10-23T17:00:00Z"},
    ],
}


class FakeFarm:
    """A farm in memory. ``calls`` records every request by its method name."""

    def __init__(self, tools: Optional[List[dict]] = None, projects: Optional[Dict[str, dict]] = None,
                 serves_submissions: bool = True, now: datetime = NOW):
        self.tools_by = {t["name"]: t for t in (tools if tools is not None else [NUKE_COMP, KARMA, TRANSCODE])}
        self.projects = projects if projects is not None else {"saltmarsh": SALTMARSH}
        self.serves_submissions = serves_submissions
        self.now = now
        self.store: Dict[str, bytes] = {}
        self.views: Dict[str, dict] = {}
        self.jobs: List[dict] = []
        self.calls: List[tuple] = []
        self.versions: Dict[str, int] = {}
        self.fail_put: Optional[str] = None

    # --- objects and views ------------------------------------------------

    def missing(self, project: str, ids: List[str]) -> List[str]:
        self.calls.append(("missing", project, len(ids)))
        seen, out = set(), []
        for i in ids:
            if i not in self.store and i not in seen:
                out.append(i)
                seen.add(i)
        return out

    def put_file(self, project: str, cid: str, path: str) -> dict:
        with open(path, "rb") as f:
            return self.put_bytes(project, cid, f.read())

    def put_bytes(self, project: str, cid: str, data: bytes) -> dict:
        self.calls.append(("put", project, cid))
        if self.fail_put == cid:
            raise FarmError(500, "store unavailable")
        if identify(data) != cid:
            raise FarmError(400, f"the body's C4 ID is not {cid}")
        stored = cid not in self.store
        self.store[cid] = data
        return {"ok": True, "id": cid, "stored": stored, "bytes": len(data)}

    def post_view(self, project: str, view: dict) -> str:
        self.calls.append(("view", project, len(view.get("files", []))))
        lacking = [f["id"] for f in view.get("files", []) if f["id"] not in self.store]
        if lacking:
            raise FarmError(409, "objects missing", {"missing": lacking})
        t = Tree()
        for f in view.get("files", []):
            t.add_file(f["path"], f["id"], f["size"], f.get("exec", False))
        for d in view.get("dirs", []):
            t.add_dir(d)
        for ln in view.get("links", []):
            t.add_link(ln["path"], ln["target"])
        vid = t.id()
        self.views[vid] = view
        return vid

    # --- tools and projects -------------------------------------------------

    def _need_slice1(self) -> None:
        if not self.serves_submissions:
            raise NotOnFarm(404, "this farm does not serve tools yet")

    def tools(self, project: str) -> List[dict]:
        self.calls.append(("tools", project))
        self._need_slice1()
        return [{"name": t["name"], "title": t.get("title", ""), "summary": t.get("summary", ""),
                 "kind": t["kind"], "version": "v1", "id": identify(json.dumps(t, sort_keys=True))}
                for t in self.tools_by.values()]

    def tool(self, project: str, name: str) -> dict:
        self.calls.append(("tool", project, name))
        self._need_slice1()
        if name not in self.tools_by:
            raise NotOnFarm(404, f"no tool {name}")
        t = self.tools_by[name]
        return {"name": name, "version": "v1", "id": identify(json.dumps(t, sort_keys=True)), "tool": t}

    def project(self, project: str) -> Optional[dict]:
        self.calls.append(("project", project))
        return self.projects.get(project)

    # --- submissions ------------------------------------------------------

    def _tool_errors(self, doc: dict) -> List[dict]:
        errs = []
        for key, job in (doc.get("jobs") or {}).items():
            name = str(job.get("tool", "")).split("@")[0]
            if name not in self.tools_by:
                errs.append(submission.error(f"jobs.{key}.tool", f'no tool "{name}" in {doc.get("project", "your project")}',
                                             "c4 farm tools lists the tools you can use"))
                continue
            tool = Tool(self.tools_by[name])
            for pname, v in (job.get("with") or {}).items():
                p = tool.param(pname)
                if p is None:
                    errs.append(submission.error(f"jobs.{key}.with.{pname}", f'{name} has no parameter "{pname}"'))
                    continue
                problem = p.check(v)
                if problem:
                    errs.append(submission.error(f"jobs.{key}.with.{pname}", problem))
            for p in tool.params:
                if p.required and p.name not in (job.get("with") or {}):
                    errs.append(submission.error(f"jobs.{key}.with.{p.name}", f"{name} needs {p.name}"))
            if job.get("size") and job["size"] not in tool.sizes:
                errs.append(submission.error(f"jobs.{key}.size", f'{name} has no size "{job["size"]}"',
                                             "sizes: " + ", ".join(tool.sizes) if tool.sizes else ""))
        return errs

    def _answer(self, doc: dict) -> dict:
        errs = submission.check(doc) + self._tool_errors(doc)
        if errs:
            return {"ok": False, "errors": errs, "estimate": None}
        due_at, source = None, "suggested"
        proj = self.projects.get(doc.get("project", ""), {})
        due = doc.get("due", "suggested")
        for m in proj.get("milestones", []):
            if m["name"] == due:
                due_at, source = m["at"], f"milestone {due}"
        jobs, finish = [], self.now
        for key, job in doc["jobs"].items():
            name = job["tool"].split("@")[0]
            tool = Tool(self.tools_by[name])
            n = len(framelib.parse(job["frames"])) if job.get("frames") else 1
            chunk = tool.frames_defaults.get("chunk", 1) if tool.takes_frames else n
            units = -(-n // chunk)
            finish = finish + timedelta(minutes=2 * units)
            v = self.versions.get(f"{doc.get('shot')}-{key}", 0) + 1
            est = {"eta50": finish.isoformat().replace("+00:00", "Z"),
                   "eta90": (finish + timedelta(minutes=units)).isoformat().replace("+00:00", "Z")}
            if due_at:
                est.update({"due": due_at, "dueSource": source,
                            "slackSeconds": int((datetime.fromisoformat(due_at.replace("Z", "+00:00"))
                                                 - (finish + timedelta(minutes=units))).total_seconds())})
            out = {"key": key, "name": f"{doc.get('shot')}-{key}-v{v:03d}", "tool": f"{name}@v1", "units": units,
                   "estimate": est}
            if tool.licences:
                out["licences"] = [{"pool": k.split("/")[0], "feature": k.split("/")[1], "perUnit": c,
                                    "free": 6, "total": 12} for k, c in tool.licences.items()]
            jobs.append(out)
        ans = {"ok": True, "jobs": jobs, "estimate": dict(jobs[-1]["estimate"]), "warnings": []}
        tree = doc.get("tree", "")
        if tree.startswith("c4") and tree not in self.views:
            ans["treeStored"] = False   # as proposed: a dry run takes a tree not uploaded yet
        return ans

    def check(self, doc: dict, explain: bool = False) -> dict:
        self.calls.append(("check", json.loads(json.dumps(doc))))
        if not self.serves_submissions:
            raise NotOnFarm(404, "this farm does not take submissions yet (POST /submissions)")
        return self._answer(doc)

    def submit(self, doc: dict) -> dict:
        self.calls.append(("submit", json.loads(json.dumps(doc))))
        if not self.serves_submissions:
            raise NotOnFarm(404, "this farm does not take submissions yet (POST /submissions)")
        tree = doc.get("tree", "")
        if tree.startswith("c4") and tree not in self.views:
            return {"ok": False, "errors": [submission.error("tree", f"no tree {tree} in the project's store",
                                                             "upload it first")]}
        ans = self._answer(doc)
        if ans["ok"]:
            for j in ans["jobs"]:
                k = f"{doc.get('shot')}-{j['key']}"
                self.versions[k] = self.versions.get(k, 0) + 1
                j["namespace"] = doc.get("project", "")
                j["link"] = f"/jobs/{j['namespace']}/{j['name']}"
            self.jobs.append(doc)
        return ans

    def submitted(self) -> List[dict]:
        return list(self.jobs)


class _Handler(BaseHTTPRequestHandler):
    farm: FakeFarm = None  # type: ignore
    token: str = ""

    def log_message(self, *a):  # quiet
        pass

    def _send(self, status: int, body: Any) -> None:
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _body(self) -> bytes:
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def _route(self, method: str) -> None:
        if self.token and self.headers.get("Authorization") != "Bearer " + self.token:
            return self._send(401, {"error": "sign in to the farm"})
        parts = [unquote(p) for p in urlparse(self.path).path.split("/") if p]
        if parts[:2] != ["api", "v1"]:
            return self._send(404, {"error": "not found"})
        p = parts[2:]
        f = self.farm
        try:
            if method == "POST" and len(p) == 3 and p[0] == "objects" and p[2] == "missing":
                return self._send(200, {"missing": f.missing(p[1], json.loads(self._body())["ids"])})
            if method == "PUT" and len(p) == 3 and p[0] == "objects":
                return self._send(200, f.put_bytes(p[1], p[2], self._body()))
            if method == "POST" and len(p) == 2 and p[0] == "views":
                return self._send(200, {"view": f.post_view(p[1], json.loads(self._body())), "files": 0, "bytes": 0})
            if method == "GET" and len(p) == 2 and p[0] == "tools":
                return self._send(200, {"tools": f.tools(p[1])})
            if method == "GET" and len(p) == 3 and p[0] == "tools":
                return self._send(200, f.tool(p[1], p[2]))
            if method == "GET" and len(p) == 2 and p[0] == "projects":
                info = f.project(p[1])
                return self._send(200, info) if info else self._send(404, {"error": "no such project"})
            if method == "POST" and p == ["submissions"]:
                req = json.loads(self._body())
                if req.get("dryRun"):
                    ans = f.check(req["submission"], req.get("explain", False))
                    return self._send(200 if ans.get("ok") else 400, ans)
                ans = f.submit(req["submission"])
                return self._send(201 if ans.get("ok") else 400, ans)
        except FarmError as e:
            return self._send(e.status or 500, e.body if isinstance(e.body, dict) else {"error": e.message})
        return self._send(404, {"error": "not found"})

    def do_GET(self):
        self._route("GET")

    def do_POST(self):
        self._route("POST")

    def do_PUT(self):
        self._route("PUT")


class Served:
    def __init__(self, server: ThreadingHTTPServer):
        self.server = server
        self.url = f"http://127.0.0.1:{server.server_address[1]}"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()


def serve(farm: FakeFarm, token: str = "") -> Served:
    """The fake farm over HTTP on a free localhost port, in a thread."""
    handler = type("Handler", (_Handler,), {"farm": farm, "token": token})
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return Served(srv)


class FakeHost(Host):
    """A scene described by data, for tests of c4submit and of panels'
    logic that does not touch the DCC."""

    name = "fake"
    stage = "comp"
    scene_word = "script"

    def __init__(self, scene: str = "", frames: str = "1001-1048", renderables: Optional[List[Renderable]] = None,
                 deps: Optional[List[Dependency]] = None, selection: Optional[List[str]] = None,
                 env: Optional[Dict[str, str]] = None, modified: bool = False):
        super().__init__(env=env or {})
        self._scene, self._frames = scene, frames
        self._renderables = renderables or []
        self._deps = deps or []
        self._selection = selection or []
        self.modified = modified
        self.saves = 0

    def scene_file(self) -> str:
        return self._scene

    def is_modified(self) -> bool:
        return self.modified

    def save(self) -> None:
        self.saves += 1
        self.modified = False

    def frames(self) -> str:
        return self._frames

    def renderables(self) -> List[Renderable]:
        return list(self._renderables)

    def selection(self) -> List[str]:
        return list(self._selection)

    def dependencies(self) -> List[Dependency]:
        return list(self._deps)


def write(path: str, data: bytes = b"x") -> str:
    """Writes a file, making its folders; returns the path."""
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)
    return path
