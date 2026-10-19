"""The c4 artifact repository, through MLflow's own artifact API."""

import mlflow
import pytest
from mlflow.exceptions import MlflowException
from mlflow.store.artifact.artifact_repository_registry import get_artifact_repository

from mlflow_c4.artifacts import C4ArtifactRepository, parse
from mlflow_c4.c4id import identify

FILES = {
    "model/MLmodel": b"flavors: {}\n",
    "model/weights/part-0.bin": b"\x00\x01" * 5000,
    "model/weights/part-1.bin": b"\x02" * 7000,
    "report/contact.png": b"png",
}


def test_registered_for_the_c4_scheme(hud):
    view = hud.add_view(FILES)
    assert isinstance(get_artifact_repository(f"c4:///{view}"), C4ArtifactRepository)


def test_download_a_subtree(hud, tmp_path):
    view = hud.add_view(FILES)
    out = mlflow.artifacts.download_artifacts(artifact_uri=f"c4:///{view}/model", dst_path=str(tmp_path))
    got = {p.relative_to(out).as_posix(): p.read_bytes() for p in __import__("pathlib").Path(out).rglob("*") if p.is_file()}
    assert got == {k[len("model/"):]: v for k, v in FILES.items() if k.startswith("model/")}


def test_both_uri_forms(hud, tmp_path):
    view = hud.add_view(FILES)
    a = mlflow.artifacts.download_artifacts(artifact_uri=f"c4://{view}/report/contact.png", dst_path=str(tmp_path / "a"))
    b = mlflow.artifacts.download_artifacts(artifact_uri=f"c4:///{view}/report/contact.png", dst_path=str(tmp_path / "b"))
    assert open(a, "rb").read() == open(b, "rb").read() == b"png"


def test_list(hud):
    view = hud.add_view(FILES)
    repo = C4ArtifactRepository(f"c4:///{view}")
    top = [(f.path, f.is_dir) for f in repo.list_artifacts()]
    assert top == [("model", True), ("report", True)]
    inner = [(f.path, f.is_dir, f.file_size) for f in repo.list_artifacts("model")]
    assert inner == [("model/MLmodel", False, len(FILES["model/MLmodel"])), ("model/weights", True, None)]


def test_corrupt_bytes_are_refused(hud, tmp_path):
    view = hud.add_view(FILES)
    cid = identify(FILES["report/contact.png"])
    hud.objects[cid] = b"not the png"
    with pytest.raises(MlflowException, match="not "):
        mlflow.artifacts.download_artifacts(artifact_uri=f"c4:///{view}/report/contact.png", dst_path=str(tmp_path))


def test_read_only(hud, tmp_path):
    repo = C4ArtifactRepository(f"c4:///{hud.add_view(FILES)}")
    f = tmp_path / "x"
    f.write_text("x")
    for call in (lambda: repo.log_artifact(str(f)), lambda: repo.log_artifacts(str(tmp_path)), repo.delete_artifacts):
        with pytest.raises(MlflowException, match="immutable"):
            call()


def test_parse():
    v = identify(b"v")
    assert parse(f"c4:///{v}") == (v, "")
    assert parse(f"c4:///{v}/model/x") == (v, "model/x")
    assert parse(f"c4://{v}/model") == (v, "model")
    for bad in ("s3://b/k", "c4:///notanid", f"c4:///{v}/../etc"):
        with pytest.raises(MlflowException):
            parse(bad)
