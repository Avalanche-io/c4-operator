"""mlflow run --backend c4, against a local MLflow tracking store and the
fake HUD."""

import textwrap

import mlflow
import pytest
from mlflow.tracking import MlflowClient

from tests.conftest import TOKEN


@pytest.fixture
def project(tmp_path):
    d = tmp_path / "denoiser"
    d.mkdir()
    (d / "MLproject").write_text(textwrap.dedent("""\
        name: denoiser
        docker_env:
          image: ghcr.io/avalanche-io/c4-job-train:dev
        entry_points:
          main:
            parameters:
              alpha: {type: float, default: 0.1}
              epochs: {type: float, default: 3}
            command: "python train.py --alpha {alpha} --epochs {epochs}"
        """))
    (d / "train.py").write_text("print('training')\n")
    (d / "mlruns").mkdir()  # never uploaded
    (d / "mlruns" / "junk").write_text("x")
    return d


@pytest.fixture
def tracking(tmp_path):
    mlflow.set_tracking_uri(f"sqlite:///{tmp_path / 'mlflow.db'}")
    exp = mlflow.create_experiment("denoiser")
    yield exp
    mlflow.set_tracking_uri(None)


DEADLINE = "2026-12-01T18:00:00Z"


def run(project, exp, **cfg):
    config = {"project": "saltmarsh", "params": {"steps": 2000}, "deadline": DEADLINE, "poll_seconds": 0.01}
    config.update(cfg)
    return mlflow.projects.run(str(project), entry_point="main", parameters={"alpha": "0.5"}, backend="c4",
                               backend_config=config, experiment_id=exp, synchronous=False, env_manager="local")


def test_submits_one_job_and_schedules_the_run(hud, project, tracking):
    sub = run(project, tracking)
    assert len(hud.jobs) == 1
    req = hud.jobs[0]
    spec = req["spec"]
    c = spec["template"]["spec"]["containers"][0]
    assert spec["class"] == "ml" and spec["params"] == {"steps": "2000"}
    assert spec["deadline"] == DEADLINE and spec["deadlineMode"] == "Tracking"
    assert c["image"] == "ghcr.io/avalanche-io/c4-job-train:dev"
    assert "python train.py --alpha 0.5 --epochs 3" in c["command"][-1]
    assert {"name": "MLFLOW_RUN_ID", "value": sub.run_id} in c["env"]
    assert req["project"] == "saltmarsh" and req["stage"] == "main"
    view = req["inputs"][0]["view"]
    assert req["inputs"][0]["mountPath"] == "/project"
    paths = sorted(f["path"] for f in hud.views[view])
    assert paths == ["MLproject", "train.py"], "mlruns is never uploaded"

    r = MlflowClient().get_run(sub.run_id)
    assert r.info.status == "SCHEDULED"
    t = r.data.tags
    assert t["c4.intake"] == "true" and t["c4.job"] == "saltmarsh-work/main-v001"
    assert t["c4.deadline"] == DEADLINE and t["c4.view"] == view
    assert t["mlflow.project.backend"] == "c4"


def test_second_run_uploads_nothing_new(hud, project, tracking):
    run(project, tracking)
    puts = hud.puts
    run(project, tracking)
    assert hud.puts == puts, "every file was already in the store"
    assert len(hud.jobs) == 2


def test_status_wait_and_cancel(hud, project, tracking):
    from mlflow.entities import RunStatus

    sub = run(project, tracking)
    name = "main-v001"
    assert sub.get_status() == RunStatus.SCHEDULED
    hud.phases[name] = "Running"
    assert sub.get_status() == RunStatus.RUNNING
    hud.phases[name] = "Succeeded"
    assert sub.wait() is True
    assert sub.get_status() == RunStatus.FINISHED
    sub.cancel()
    assert hud.nudges == [(name, {"holdStop": True})]


def test_failed_job(hud, project, tracking):
    sub = run(project, tracking)
    hud.phases["main-v001"] = "Failed"
    assert sub.wait() is False


def test_needs_a_project_and_an_image(hud, project, tracking):
    with pytest.raises(Exception, match="project"):
        run(project, tracking, project="")
    (project / "MLproject").write_text("name: x\nentry_points:\n  main:\n    command: echo\n")
    with pytest.raises(Exception, match="image"):
        mlflow.projects.run(str(project), entry_point="main", backend="c4",
                            backend_config={"project": "saltmarsh"}, experiment_id=tracking,
                            synchronous=False, env_manager="local")


def test_bad_deadline_refused_before_submitting(hud, project, tracking):
    with pytest.raises(ValueError):
        run(project, tracking, deadline="next tuesday")
    assert hud.jobs == []


def test_unauthorized(hud, project, tracking, monkeypatch):
    monkeypatch.setenv("C4_HUD_TOKEN", "wrong")
    with pytest.raises(Exception, match="401"):
        run(project, tracking)
    assert TOKEN != "wrong"
