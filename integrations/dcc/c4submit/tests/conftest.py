import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

import pytest  # noqa: E402

from c4submit.collect import NoCache  # noqa: E402


@pytest.fixture(autouse=True)
def _no_user_cache(monkeypatch, tmp_path):
    # Tests never read or write the artist's ID cache.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))


@pytest.fixture
def nocache():
    return NoCache()
