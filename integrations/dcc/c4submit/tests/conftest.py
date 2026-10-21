import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

import pytest  # noqa: E402

from c4submit.collect import NoCache  # noqa: E402


@pytest.fixture(autouse=True)
def _no_user_cache(monkeypatch, tmp_path):
    # Tests never read or write the artist's ID cache.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))
    # Nor the farms `c4 farm` knows on this machine.
    monkeypatch.setenv("C4_FARM_CONFIG", str(tmp_path / "no-farm.yaml"))
    for k in ("C4_FARM", "C4_HUD_AS"):
        monkeypatch.delenv(k, raising=False)


@pytest.fixture
def nocache():
    return NoCache()
