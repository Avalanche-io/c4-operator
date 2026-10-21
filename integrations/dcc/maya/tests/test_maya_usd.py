"""MayaHost follows a USD stage (a proxy shape, a reference) into what it reads in turn."""

import os

import fakemaya as fm
import pytest
from c4submit import usd as usdlib
from c4submit.collect import gather
from c4submit.testing import write
from maya_scenes import light_scene

MARSH = '''#usda 1.0
(
    subLayers = [@./marsh_layout.usda@]
)
def Xform "marsh" (
    references = @reeds/reeds.usd@
)
{
    asset mud = @tex/mud.<UDIM>.exr@
}
'''


@pytest.fixture(autouse=True)
def _no_pxr(monkeypatch):
    # As in a Maya without USD's Python: the text layers are read here.
    monkeypatch.setattr(usdlib, "_with_pxr", lambda path: None)


def marsh_files(show):
    marsh = os.path.join(show, "assets", "marsh")
    stage = write(os.path.join(marsh, "marsh_set.usda"), MARSH.encode())
    write(os.path.join(marsh, "marsh_layout.usda"), b'#usda 1.0\ndef "rocks" { asset a = @rocks.abc@ }\n')
    write(os.path.join(marsh, "rocks.abc"), b"abc")
    write(os.path.join(marsh, "reeds", "reeds.usd"), b"PXR-USDC binary")
    for tile in (1001, 1002):
        write(os.path.join(marsh, "tex", f"mud.{tile}.exr"), b"mud %d" % tile)
    return stage


def rel(show, d):
    return os.path.relpath(d.path, show).replace(os.sep, "/")


def test_a_proxy_shapes_stage_and_what_it_reads_tagged_by_its_layers(tmp_path):
    host, _ = light_scene(tmp_path)
    show = str(tmp_path)
    stage = marsh_files(show)
    fm.usd_proxy("marsh", stage)
    fm.state().members["rs_fg"].append("marsh")   # the marsh is in fg only
    deps = host.dependencies()
    proxy = [d for d in deps if d.node == "marshShape"]
    assert [d.path for d in proxy] == [stage] and proxy[0].renderables == ["rs_fg"]
    via = {rel(show, d): d for d in deps if d.node == "marshShape > marsh_set.usda"}
    assert set(via) == {"assets/marsh/marsh_layout.usda", "assets/marsh/rocks.abc", "assets/marsh/reeds/reeds.usd",
                        "assets/marsh/tex/mud.<UDIM>.exr"}
    assert all(d.renderables == ["rs_fg"] for d in via.values()), "as the proxy shape: only fg reads the marsh"
    assert host.warnings() == [
        os.path.join(show, "assets", "marsh", "reeds", "reeds.usd") + " is binary USD and USD's Python is not "
        "available here, so what it reads in turn was not collected"]
    beauty = [d for d in deps if d.renderables is None or "rs_beauty" in d.renderables]
    assert not any("marsh" in d.path for d in beauty), "unticking fg leaves out the marsh and all it reads"


def test_a_relative_proxy_path_is_from_the_scenes_folder(tmp_path):
    host, scene = light_scene(tmp_path)
    show = str(tmp_path)
    stage = write(os.path.join(os.path.dirname(scene), "usd", "shot.usda"), b'#usda 1.0\ndef "x" { asset a = @shot.abc@ }\n')
    write(os.path.join(os.path.dirname(stage), "shot.abc"), b"abc")
    fm.usd_proxy("shot", "usd/shot.usda", relative=True)
    deps = {d.node: d for d in host.dependencies()}
    assert deps["shotShape"].path == stage, "the plug-in's relative path is from the scene file, not the workspace"
    assert deps["shotShape > shot.usda"].path == os.path.join(os.path.dirname(stage), "shot.abc")
    col = gather(show, scene, host.dependencies())
    assert col.problems == [] and "seq/sm0140/maya/scenes/usd/shot.abc" in col.files
    assert host.warnings() == []


def test_a_reference_to_a_usd_file_brings_what_it_reads_for_every_layer(tmp_path):
    host, _ = light_scene(tmp_path)
    show = str(tmp_path)
    stage = marsh_files(show)
    fm.reference("marshRN", stage, loaded=False)
    deps = host.dependencies()
    via = [d for d in deps if d.node == "marshRN > marsh_set.usda"]
    assert sorted(rel(show, d) for d in via) == ["assets/marsh/marsh_layout.usda", "assets/marsh/reeds/reeds.usd",
                                                 "assets/marsh/rocks.abc", "assets/marsh/tex/mud.<UDIM>.exr"]
    assert all(d.renderables is None for d in via), "a reference is read whenever the scene opens"
    assert all(d.optional for d in via), "an unloaded reference's files are warnings when missing, as it is"


def test_a_scene_without_usd_has_no_warnings(tmp_path):
    host, _ = light_scene(tmp_path)
    fm.usd_proxy("gone", str(tmp_path / "assets" / "gone.usda"))
    deps = {d.node: d for d in host.dependencies()}
    assert "gone > gone.usda" not in {d.node for d in host.dependencies()}, "a missing stage reads nothing in turn"
    assert deps["goneShape"].path == str(tmp_path / "assets" / "gone.usda"), "it is still a dependency, reported missing"
    assert host.warnings() == []
