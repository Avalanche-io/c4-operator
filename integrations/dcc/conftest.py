"""For every DCC folder's tests: never read the farms `c4 farm` knows on
the machine running them (``~/.c4/farm.yaml``), so a test sees only the
farm it sets up."""

import pytest


@pytest.fixture(autouse=True)
def _no_farm_config(monkeypatch, tmp_path):
    monkeypatch.setenv("C4_FARM_CONFIG", str(tmp_path / "no-farm.yaml"))
    for k in ("C4_FARM", "C4_HUD_AS"):
        monkeypatch.delenv(k, raising=False)
