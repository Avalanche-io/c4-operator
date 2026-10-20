import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
for p in (HERE, os.path.join(HERE, "..", "python"), os.path.join(HERE, "..", "..", "c4submit", "src")):
    p = os.path.normpath(p)
    if p not in sys.path:
        sys.path.insert(0, p)

import fakehou  # noqa: E402

fakehou.install()

import pytest  # noqa: E402

from c4submit.collect import NoCache  # noqa: E402


@pytest.fixture(autouse=True)
def _session(monkeypatch, tmp_path):
    # Tests never read the artist's ID cache or the studio's environment.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))
    for k in ("C4_HUD", "C4_HUD_TOKEN", "C4_HUD_TOKEN_FILE", "C4_PROJECT", "C4_ROOT", "C4_SHOT", "SHOT",
              "C4_SHOT_PATTERN", "HFS", "C4_DCC"):
        monkeypatch.delenv(k, raising=False)
    fakehou.reset()
    from c4_houdini import nodes
    nodes._panels.clear()
    yield
    nodes._panels.clear()


@pytest.fixture
def nocache():
    return NoCache()
