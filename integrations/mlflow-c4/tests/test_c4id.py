import io

import pytest

from mlflow_c4.c4id import encode, identify, is_id


def test_matches_the_go_library():
    # The empty input is the Go library's own test vector (c4 id_test.go,
    # TestNILID); "hello" was computed with c4.Identify on 2026-10-19.
    assert identify(b"") == "c459dsjfscH38cYeXXYogktxf4Cd9ibshE3BHUo6a58hBXmRQdZrAkZzsWcbWtDg5oQstpDuni4Hirj75GEmTc1sFT"
    assert identify(b"hello") == "c447Fm3BJZQ62765jMZJH4m28hrDM7Szbj9CUmj4F4gnvyDYXYz4WfnK2nYRhFvRgYEectEXYBYWLDpLo6XGNAfKdt"


def test_stream_equals_bytes():
    data = bytes(range(256)) * 9000
    assert identify(io.BytesIO(data)) == identify(data)


def test_shape():
    assert is_id(identify(b"x"))
    assert len(identify(b"x")) == 90
    assert not is_id("c4" + "0" * 88)  # 0 is not in the base58 alphabet
    assert encode(b"\x00" * 64) == "c4" + "1" * 88
    with pytest.raises(ValueError):
        encode(b"short")
