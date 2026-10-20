"""A real MLflow server (localhost only) and c4 model-version sources.

The open question: does a server accept c4:// as a model-version source,
with and without this plugin?
Measured against MLflow 3.17.1 on 2026-10-19:

- the server stores a model-version source without resolving its scheme,
  so the plugin is not needed server-side to register one (the same
  results came from a server without the plugin installed);
- it refuses a source whose path is empty, so ``c4://<view>`` and
  ``c4://<view>/`` are refused, and ``c4:///<view>``, ``c4:///<view>/x``
  and ``c4://<view>/x`` are accepted;
- clients need the plugin to read such a version (models:/ resolves to
  the c4 source).

This test repeats the measurement with the plugin installed, and reads
the registered version back through models:/. Set MLFLOW_C4_NO_SERVER=1
to skip it.
"""

import json
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

import mlflow
import pytest

pytestmark = pytest.mark.skipif(os.environ.get("MLFLOW_C4_NO_SERVER") == "1", reason="MLFLOW_C4_NO_SERVER=1")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture
def server(tmp_path):
    port = free_port()
    mlflow_bin = shutil.which("mlflow", path=os.path.dirname(sys.executable)) or "mlflow"
    env = dict(os.environ, MLFLOW_DISABLE_AGENT_HINT="1")
    proc = subprocess.Popen(
        [mlflow_bin, "server", "--host", "127.0.0.1", "--port", str(port),
         "--backend-store-uri", f"sqlite:///{tmp_path / 'srv.db'}", "--default-artifact-root", str(tmp_path / "art")],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
    url = f"http://127.0.0.1:{port}"
    deadline = time.monotonic() + 90
    while True:  # wait for the condition, with a deadline: never a fixed sleep
        try:
            with urllib.request.urlopen(url + "/health", timeout=2) as r:
                if r.status == 200:
                    break
        except (urllib.error.URLError, ConnectionError, OSError):
            pass
        if proc.poll() is not None:
            pytest.fail(f"mlflow server exited with {proc.returncode}")
        if time.monotonic() > deadline:
            proc.terminate()
            proc.wait(10)
            pytest.fail("mlflow server did not answer /health within 90 s")
        time.sleep(0.2)
    yield url
    proc.terminate()
    try:
        proc.wait(15)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()


def post(url, path, body):
    req = urllib.request.Request(url + "/api/2.0/mlflow/" + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)


FILES = {"MLmodel": b"artifact_path: model\nflavors: {}\n", "weights.bin": b"\x07" * 4096}


def test_c4_sources(server, hud, tmp_path):
    view = hud.add_view(FILES)
    assert post(server, "registered-models/create", {"name": "denoiser"})[0] == 200
    refused = [f"c4://{view}", f"c4://{view}/"]
    accepted = [f"c4:///{view}", f"c4:///{view}/model", f"c4://{view}/model"]
    for src in refused:
        code, body = post(server, "model-versions/create", {"name": "denoiser", "source": src})
        assert code == 400 and body["error_code"] == "INVALID_PARAMETER_VALUE", (src, body)
    versions = []
    for src in accepted:
        code, body = post(server, "model-versions/create", {"name": "denoiser", "source": src})
        assert code == 200, (src, body)
        versions.append(body["model_version"]["version"])

    # A client with the plugin reads version 1 (source c4:///<view>)
    # through models:/, which resolves to the c4 source.
    mlflow.set_tracking_uri(server)
    try:
        out = mlflow.artifacts.download_artifacts(artifact_uri=f"models:/denoiser/{versions[0]}", dst_path=str(tmp_path / "dl"))
    finally:
        mlflow.set_tracking_uri(None)
    got = {name: open(os.path.join(out, name), "rb").read() for name in sorted(os.listdir(out))}
    # MLflow adds registered_model_meta to a models:/ download itself.
    got.pop("registered_model_meta", None)
    assert got == FILES
