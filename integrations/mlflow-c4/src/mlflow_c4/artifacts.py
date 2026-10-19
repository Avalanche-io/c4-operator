"""The ``c4`` artifact repository: MLflow reads artifacts that live in a C4
view, by the view's C4 ID.

URI forms (both name the same thing):

- ``c4:///<view>[/<path>]``, the canonical form, which an MLflow server
  accepts as a model-version source even for a whole view;
- ``c4://<view>/<path>``. An MLflow server refuses ``c4://<view>`` with no
  path as a model-version source (its source check wants a non-empty
  absolute path; measured against MLflow 3.17.1), so a whole view is
  written ``c4:///<view>``.

The repository reads through the HUD API (``GET /views/{project}/{view}/
files``, ``GET /objects/{project}/{id}``) as the caller, in the project
``C4_PROJECT`` names, and checks every downloaded file against its C4 ID.
It is read-only: a view is immutable, and what a C4 job makes is published
by the farm, not logged here.
"""

import os
import posixpath
import urllib.parse
from typing import Optional

from mlflow.entities import FileInfo
from mlflow.exceptions import MlflowException
from mlflow.store.artifact.artifact_repo import ArtifactRepository

from mlflow_c4.c4id import identify, is_id
from mlflow_c4.hud import HUD


def parse(uri: str):
    """(view, path inside the view) of a c4 artifact URI."""
    u = urllib.parse.urlparse(uri)
    if u.scheme != "c4":
        raise MlflowException(f"not a c4 artifact URI: {uri}")
    path = u.path.strip("/")
    if u.netloc:
        view, rest = u.netloc, path
    else:
        view, _, rest = path.partition("/")
    if not is_id(view):
        raise MlflowException(f"{uri}: {view!r} is not a C4 ID")
    if ".." in rest.split("/"):
        raise MlflowException(f"{uri}: relative path references are not allowed")
    return view, rest


class C4ArtifactRepository(ArtifactRepository):
    def __init__(self, artifact_uri: str, tracking_uri: Optional[str] = None,
                 registry_uri: Optional[str] = None) -> None:
        super().__init__(artifact_uri, tracking_uri=tracking_uri, registry_uri=registry_uri)
        self.view, self.root = parse(artifact_uri)
        self.project = os.environ.get("C4_PROJECT", "")
        if not self.project:
            raise MlflowException("C4_PROJECT names the C4 project whose store holds the view")
        self.hud = HUD()
        self._files = None

    def _listing(self):
        """Every file under the root: {relative path: (id, size)}."""
        if self._files is None:
            files = self.hud.view_files(self.project, self.view, self.root)
            out = {}
            for f in files:
                p = f["path"]
                if self.root:
                    if p == self.root:
                        p = posixpath.basename(p)
                    elif p.startswith(self.root + "/"):
                        p = p[len(self.root) + 1:]
                    else:
                        continue
                out[p] = (f["id"], f.get("size"))
            self._files = out
        return self._files

    def list_artifacts(self, path: Optional[str] = None):
        prefix = (path or "").strip("/")
        seen, out = set(), []
        for p, (_, size) in sorted(self._listing().items()):
            if prefix and not (p == prefix or p.startswith(prefix + "/")):
                continue
            rest = p[len(prefix) + 1:] if prefix else p
            if not rest:  # path names a file
                continue
            head, sep, _ = rest.partition("/")
            child = posixpath.join(prefix, head) if prefix else head
            if child in seen:
                continue
            seen.add(child)
            out.append(FileInfo(child, True, None) if sep else FileInfo(child, False, size))
        return out

    def _download_file(self, remote_file_path: str, local_path: str) -> None:
        entry = self._listing().get(remote_file_path.strip("/"))
        if entry is None:
            raise MlflowException(f"{remote_file_path} is not in view {self.view}", error_code="RESOURCE_DOES_NOT_EXIST")
        cid, _ = entry
        data = self.hud.get_object(self.project, cid)
        if identify(data) != cid:
            raise MlflowException(f"{remote_file_path}: the bytes received are not {cid}")
        with open(local_path, "wb") as f:
            f.write(data)

    def log_artifact(self, local_file, artifact_path=None):
        raise MlflowException("c4:// views are immutable: a C4 job's output is published by the farm (c4-sync)")

    def log_artifacts(self, local_dir, artifact_path=None):
        raise MlflowException("c4:// views are immutable: a C4 job's output is published by the farm (c4-sync)")

    def delete_artifacts(self, artifact_path=None):
        raise MlflowException("c4:// views are immutable and cannot be deleted through MLflow")
