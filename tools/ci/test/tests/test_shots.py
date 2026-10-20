"""The ci tool's test repository: Saltmarsh's pipeline-tools sample, trimmed,
without the sample's deliberately flaky test (a tool's test must pass every
time, or the tool cannot be published)."""

import pytest

from pftools import shots


def test_parse():
    assert shots.parse("sm0140") == ("sm", 140)


def test_parse_rejects():
    with pytest.raises(ValueError):
        shots.parse("shot140")


def test_review_name():
    assert shots.review_name("sm0140", 3) == "sm0140_review_v003.mov"


def test_frames():
    assert len(shots.frames("1001-1048")) == 48
