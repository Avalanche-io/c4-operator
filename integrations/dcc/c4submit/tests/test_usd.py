"""The files a USD file reads in turn, without USD's own Python."""

import os

from c4submit import usd
from c4submit.collect import gather
from c4submit.testing import write

SHOT = '''#usda 1.0
(
    subLayers = [@./layout.usda@, @lighting/key.usda@]
)
def Xform "set" (
    references = @assets/set.usd@
)
{
    asset texture = @./tex/wood.<UDIM>.exr@
    asset cache = @@@caches/fx.$F4.vdb@@@
    asset web = @https://example.com/x.exr@
}
'''


def test_layers_references_and_assets(tmp_path):
    root = str(tmp_path)
    shot = write(os.path.join(root, "sh010.usda"), SHOT.encode())
    write(os.path.join(root, "layout.usda"), b'#usda 1.0\ndef "x" { asset a = @layout_cache.abc@ }\n')
    write(os.path.join(root, "lighting/key.usda"), b'#usda 1.0\ndef "k" { asset ies = @../ies/key.ies@ }\n')
    write(os.path.join(root, "assets/set.usd"), b"PXR-USDC binary")
    deps, unread = usd.dependencies(shot, use_pxr=False)
    got = sorted(os.path.relpath(d.path, root).replace(os.sep, "/") for d in deps)
    assert got == ["assets/set.usd", "caches/fx.$F4.vdb", "ies/key.ies", "layout.usda", "layout_cache.abc",
                   "lighting/key.usda", "tex/wood.<UDIM>.exr"]
    assert [os.path.basename(u) for u in unread] == ["set.usd"], "a binary layer is sent but not read"
    assert {d.node for d in deps} == {"sh010.usda"}


def test_collected_with_tiles_and_frames(tmp_path):
    root = str(tmp_path)
    shot = write(os.path.join(root, "sh010.usda"),
                 b'#usda 1.0\ndef "s" {\n asset t = @tex/rock.<UDIM>.exr@\n asset c = @fx/smoke.$F4.vdb@\n}\n')
    for t in (1001, 1002):
        write(os.path.join(root, f"tex/rock.{t}.exr"))
    for f in (1, 2, 3):
        write(os.path.join(root, f"fx/smoke.{f:04d}.vdb"))
    deps, _ = usd.dependencies(shot, use_pxr=False)
    col = gather(root, shot, deps)
    assert sorted(col.files) == ["fx/smoke.0001.vdb", "fx/smoke.0002.vdb", "fx/smoke.0003.vdb", "sh010.usda",
                                 "tex/rock.1001.exr", "tex/rock.1002.exr"]


def test_a_cycle_of_sublayers_ends(tmp_path):
    root = str(tmp_path)
    a = write(os.path.join(root, "a.usda"), b'#usda 1.0\n(\n subLayers = [@b.usda@]\n)\n')
    write(os.path.join(root, "b.usda"), b'#usda 1.0\n(\n subLayers = [@a.usda@]\n)\n')
    deps, _ = usd.dependencies(a, use_pxr=False)
    assert [os.path.basename(d.path) for d in deps] == ["b.usda"]


def test_with_usds_own_python(tmp_path, monkeypatch):
    import sys
    import types
    shot = write(os.path.join(str(tmp_path), "sh010.usda"), b"#usda 1.0\n")
    seen = {}

    class Layer:
        def __init__(self, path, anonymous=False):
            self.realPath, self.identifier, self.anonymous = path, path, anonymous

    def compute(asset):
        seen["asset"] = asset
        return ([Layer(shot), Layer(os.path.join(str(tmp_path), "lay.usd")), Layer("anon:0x1", True)],
                [os.path.join(str(tmp_path), "tex", "a.<UDIM>.exr")], ["missing/ref.usda"])

    pxr = types.ModuleType("pxr")
    pxr.Sdf = types.SimpleNamespace(AssetPath=lambda p: ("asset", p))
    pxr.UsdUtils = types.SimpleNamespace(ComputeAllDependencies=compute)
    monkeypatch.setitem(sys.modules, "pxr", pxr)
    deps, unread = usd.dependencies(shot, node="/stage/LOP")
    assert seen["asset"] == ("asset", shot) and unread == []
    got = [os.path.relpath(d.path, str(tmp_path)).replace(os.sep, "/") for d in deps]
    assert got == ["lay.usd", "tex/a.<UDIM>.exr", "missing/ref.usda"]
    assert {d.node for d in deps} == {"/stage/LOP"}
