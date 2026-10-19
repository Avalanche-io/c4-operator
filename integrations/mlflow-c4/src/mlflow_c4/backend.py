"""``mlflow run --backend c4``: an MLflow project run as a C4 job.

The backend stores the project's files in the C4 project's store as a view
(only the files the store lacks are uploaded), creates the MLflow run in
status SCHEDULED, and submits one WorkJob through the HUD API whose units
run the entry point's command in the project's image, with the run's id
and the tracking URI in their environment. The run is tagged so c4-sync
adopts it: ``c4.intake=true`` and ``c4.job=<namespace>/<name>`` (c4-sync's
MLflow adapter links the run to that job instead of submitting another),
plus ``c4.deadline`` when one is given. From then on the run's status and
``c4.*`` tags follow the job (c4-sync), and each output is registered as a
model version whose source is ``c4:///<view>``.

Backend config (``--backend-config`` JSON, or a dict):

==================  ============================================================
key                 meaning
==================  ============================================================
hud                 the HUD's URL (else ``C4_HUD``)
token_file          a file holding the caller's bearer token (else
                    ``C4_HUD_TOKEN_FILE`` or ``C4_HUD_TOKEN``)
project             the C4 project whose store holds the project's files
namespace           the project namespace for the job (else the project's first)
image               the container image (else the MLproject's ``docker_env``)
class               the work class, default ``ml``
params              the class's parameters (``steps``, ``ranks``,
                    ``checkpointEvery`` for ml), strings
deadline            the job's deadline, RFC 3339; without one the farm
                    suggests one (C4 is deadline-driven: say when it matters)
pools               the pools the job may run in, a list
mount               where the project's files are mounted, default ``/project``
poll_seconds        how often ``wait()`` reads the job, default 15
==================  ============================================================
"""

import json
import os
import shlex
import tempfile
import time
from datetime import datetime, timezone
from typing import Optional

from mlflow.entities import RunStatus
from mlflow.projects.backend.abstract_backend import AbstractBackend
from mlflow.projects.submitted_run import SubmittedRun
from mlflow.projects.utils import fetch_and_validate_project, get_or_create_run, load_project
from mlflow.tracking import MlflowClient

from mlflow_c4.c4id import identify
from mlflow_c4.hud import HUD

SKIP_DIRS = {".git", "mlruns", "__pycache__", ".venv", ".mypy_cache", ".pytest_cache"}

# The HUD's job phases (WorkJob status.phase) as MLflow run statuses.
PHASE_STATUS = {
    "Queued": "SCHEDULED",
    "Blocked": "SCHEDULED",
    "Held": "SCHEDULED",
    "Running": "RUNNING",
    "Succeeded": "FINISHED",
    "Failed": "FAILED",
}


def project_files(work_dir: str):
    """The project's files, relative to work_dir, slash-separated, sorted."""
    out = []
    for root, dirs, files in os.walk(work_dir):
        dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
        for f in sorted(files):
            full = os.path.join(root, f)
            if os.path.islink(full):
                continue
            rel = os.path.relpath(full, work_dir).replace(os.sep, "/")
            out.append((rel, full))
    return out


def store_view(hud: HUD, project: str, work_dir: str) -> str:
    """Stores the directory as a view and returns its C4 ID, uploading only
    what the store lacks (POST /objects/{project}/missing first)."""
    files, by_id = [], {}
    for rel, full in project_files(work_dir):
        with open(full, "rb") as f:
            data = f.read()
        cid = identify(data)
        by_id[cid] = full
        files.append({"path": rel, "id": cid, "size": len(data),
                      "exec": bool(os.stat(full).st_mode & 0o111)})
    ids = list(by_id)
    for i in range(0, len(ids), 100000):
        for cid in hud.missing(project, ids[i:i + 100000]):
            with open(by_id[cid], "rb") as f:
                hud.put_object(project, cid, f.read())
    return hud.post_view(project, files)


def _config(backend_config) -> dict:
    if isinstance(backend_config, str):
        with open(backend_config, encoding="utf-8") as f:
            return json.load(f)
    return dict(backend_config or {})


class C4SubmittedRun(SubmittedRun):
    """The job behind an MLflow run, read through the HUD."""

    def __init__(self, hud: HUD, namespace: str, name: str, run_id: str, poll_seconds: float = 15):
        super().__init__()
        self.hud, self.namespace, self.name = hud, namespace, name
        self._run_id = run_id
        self.poll_seconds = poll_seconds

    @property
    def run_id(self):
        return self._run_id

    def _phase(self) -> str:
        return self.hud.job(self.namespace, self.name).get("phase", "")

    def wait(self):
        while True:
            phase = self._phase()
            if phase in ("Succeeded", "Failed"):
                return phase == "Succeeded"
            time.sleep(self.poll_seconds)

    def get_status(self):
        return RunStatus.from_string(PHASE_STATUS.get(self._phase(), "SCHEDULED"))

    def cancel(self):
        # The HUD has no cancel: hold the job and stop its running units
        # (nudge holdStop), which a person can release.
        self.hud.nudge(self.namespace, self.name, {"holdStop": True})


class C4Backend(AbstractBackend):
    def run(self, project_uri, entry_point, params, version, backend_config, tracking_uri, experiment_id):
        cfg = _config(backend_config)
        hud = HUD(base=cfg.get("hud"), token_file=cfg.get("token_file"))
        project_name = cfg.get("project") or os.environ.get("C4_PROJECT")
        if not project_name:
            raise ValueError("backend config 'project' (the C4 project) is required, or C4_PROJECT")

        work_dir = fetch_and_validate_project(project_uri, version, entry_point, params)
        project = load_project(work_dir)
        image = cfg.get("image") or (project.docker_env or {}).get("image")
        if not image:
            raise ValueError("an image is required: backend config 'image', or docker_env in MLproject")
        storage_dir = tempfile.mkdtemp(prefix="mlflow-c4-")
        command = project.get_entry_point(entry_point).compute_command(params, storage_dir)

        view = store_view(hud, project_name, work_dir)
        active = get_or_create_run(None, project_uri, experiment_id, work_dir, version, entry_point, params)
        run_id = active.info.run_id
        client = MlflowClient()
        client.update_run(run_id, status="SCHEDULED")

        mount = cfg.get("mount", "/project")
        env = [{"name": "MLFLOW_RUN_ID", "value": run_id},
               {"name": "MLFLOW_EXPERIMENT_ID", "value": str(experiment_id)}]
        if tracking_uri and tracking_uri.split(":", 1)[0] in ("http", "https", "databricks"):
            env.append({"name": "MLFLOW_TRACKING_URI", "value": tracking_uri})
        spec = {
            "class": cfg.get("class", "ml"),
            "params": {k: str(v) for k, v in (cfg.get("params") or {}).items()},
            "template": {"spec": {"containers": [{
                "name": "mlflow", "image": image, "workingDir": mount,
                "command": ["sh", "-c", f"cd {shlex.quote(mount)} && {command}"], "env": env,
            }]}},
        }
        deadline = cfg.get("deadline")
        if deadline:
            datetime.fromisoformat(deadline.replace("Z", "+00:00"))  # refuse a malformed time here
            spec["deadline"], spec["deadlineMode"] = deadline, "Tracking"
        if cfg.get("pools"):
            spec["pools"] = list(cfg["pools"])
        req = {"spec": spec, "type": "mlflow", "stage": entry_point,
               "inputs": [{"view": view, "mountPath": mount}]}
        if cfg.get("namespace"):
            req["namespace"] = cfg["namespace"]
        else:
            req["project"] = project_name
        try:
            out = hud.submit(req)
        except Exception:
            client.set_terminated(run_id, status="FAILED")
            raise
        ns, name = out["namespace"], out["name"]
        tags = {"c4.intake": "true", "c4.job": f"{ns}/{name}", "c4.view": view,
                "c4.submitted": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")}
        if deadline:
            tags["c4.deadline"] = deadline
        for k, v in tags.items():
            client.set_tag(run_id, k, v)
        return C4SubmittedRun(hud, ns, name, run_id, float(cfg.get("poll_seconds", 15)))
