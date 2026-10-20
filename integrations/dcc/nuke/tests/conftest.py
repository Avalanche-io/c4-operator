import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(HERE, "..", "..", "c4submit", "src"))

import fakenuke  # noqa: E402

fakenuke.install()

import pytest  # noqa: E402


@pytest.fixture(autouse=True)
def _fresh(monkeypatch, tmp_path):
    # Tests never read or write the artist's ID cache, or reach a real farm.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))
    for k in ("C4_HUD", "C4_HUD_TOKEN", "C4_HUD_TOKEN_FILE", "C4_PROJECT", "C4_SHOT", "C4_SHOT_PATTERN", "C4_ROOT", "SHOT"):
        monkeypatch.delenv(k, raising=False)
    fakenuke.reset()
    yield
    fakenuke.reset()
