"""Regressions for the bugs an adversarial review of c4submit found on
2026-10-20, one test per finding."""

import math
import os
import socket
import subprocess
import threading

import pytest
import yaml

from c4submit import collect, paths, seq, yamlio
from c4submit.farm import Farm, FarmError
from c4submit.host import Dependency, Renderable
from c4submit.submitter import Draft
from c4submit.testing import FakeHost, NUKE_COMP, write
from c4submit.tool import Tool
from test_ids import OPERATOR, _scan

# Sibling folders where one name is a prefix of the other: c4m sorts
# directory names with their trailing "/", so "a1/" comes before "a/".
# The IDs are `c4-operator tree-id`'s at c4-operator 06ae785.
PREFIX_DIRS = {
    ("a", "a1"): "c42dfnpCdF1mngg4UG8iTT9pUv4pUgqP3WpgWD9Hnw7mxf3tANYSV39FGJth4Ky5i9Phij6sfJPF4Tou6CMsmG4AHQ",
    ("shot", "shot.v2"): "c44BuXAS6s1JHnw993hwAnDWjzGJQDYuhcpuyadmQBmLCw3zvw4Q3NRJGMUkLMFKxSRqTcLNBgMxnhjS7wJpmscusB",
    ("plate", "plate 2"): "c44F6p95KcVjXGTtACWBgHcEnjSvSojrkCavXCwGd8AM7F9SXG9jwnNoi4rr1s5b223mRjjoKKExmCMaz571BMHW8M",
}


@pytest.mark.parametrize("names", list(PREFIX_DIRS), ids=lambda n: "+".join(n))
def test_directory_order_matches_the_operator(tmp_path, names):
    for n in names:
        write(os.path.join(str(tmp_path), n, "f"), n.encode())
    got = _scan(str(tmp_path)).id()
    assert got == PREFIX_DIRS[names]
    if OPERATOR:
        out = subprocess.run([OPERATOR, "tree-id", str(tmp_path)], capture_output=True, text=True, check=True)
        assert got == out.stdout.strip()


@pytest.mark.parametrize("v", [1e16, 1e-7, 1e300, float("nan"), float("inf"), -float("inf"), 0.1, -0.0,
                               "1.2_3", "1_0:30", "1_0:30.5", "a\x7fb", "a\x80b", "\ufffe", "a\u0085b", "\u2028"])
def test_yaml_round_trips(v):
    doc = {"k": v, "l": [v], "m": {"a": v}}
    back = yaml.safe_load(yamlio.dump(doc))
    for got in (back["k"], back["l"][0], back["m"]["a"]):
        if isinstance(v, float) and math.isnan(v):
            assert isinstance(got, float) and math.isnan(got)
        else:
            assert type(got) == type(v) and got == v


def test_farm_failures_are_farm_errors(tmp_path):
    # A server that accepts and never answers.
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    s.listen(1)
    try:
        with pytest.raises(FarmError) as e:
            Farm(base=f"http://127.0.0.1:{s.getsockname()[1]}", timeout=0.5).tools("p")
        assert e.value.status == 0
    finally:
        s.close()
    with pytest.raises(FarmError) as e:
        Farm(base="http://127.0.0.1:9", token_file=str(tmp_path / "no-token")).tools("p")
    assert "token file" in e.value.message


def test_a_sign_in_page_is_a_farm_error():
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    class Page(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            body = b"<html>sign in</html>"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    srv = ThreadingHTTPServer(("127.0.0.1", 0), Page)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        with pytest.raises(FarmError) as e:
            Farm(base=f"http://127.0.0.1:{srv.server_address[1]}").tools("p")
        assert "not the farm's JSON" in e.value.message
    finally:
        srv.shutdown()
        srv.server_close()


def test_a_bad_dependency_range_is_a_problem(tmp_path):
    scene = write(str(tmp_path / "a.nk"))
    col = collect.gather(str(tmp_path), scene, [Dependency(str(tmp_path / "p.####.exr"), "1001-1100, 1200x", node="Read1")])
    assert col.problems[0].path == "Read1" and "do not read" in col.problems[0].message


def test_long_keys_stay_keys(tmp_path):
    long = "Write_beauty_main_character_hero_pass_left_"
    host = FakeHost(scene=write(str(tmp_path / "a.nk")), renderables=[Renderable(long + "A"), Renderable(long + "B")],
                    env={"C4_ROOT": str(tmp_path)})
    keys = list(Draft(host, Tool(NUKE_COMP)).keys())
    assert len(set(keys)) == 2 and all(len(k) <= 40 for k in keys)


def test_windows_paths_in_sequences():
    assert seq.matches("S:/shows/x/tex.<UDIM>.exr", "S:/shows/x\\tex.1001.exr")
    assert seq.canonical("S:\\Shows\\X\\out.####.exr") == seq.canonical("s:/shows/x/out.%04d.exr")


def test_frame_tokens_edge_cases():
    assert not seq.has_frames("img.$F4_denoised.exr")
    assert seq.at_frame("x.%04d.exr", -5) == "x.-005.exr"
    assert seq.canonical("o.#.exr") == seq.canonical("o.%d.exr") == seq.canonical("o.$F.exr")


def test_relative_to_the_root_itself_and_to_slash():
    assert paths.relative("/show", "/show") == "."
    assert paths.relative("/", "/show/a.exr") == "show/a.exr"
    assert paths.relative("S:/Shows", "s:\\shows\\a.exr") == "a.exr"


def test_a_tree_path_is_one_thing():
    from c4submit.tree import Tree
    t = Tree()
    t.add_file("a", "c4" + "1" * 88, 1)
    with pytest.raises(ValueError):
        t.add_link("a", "b")
