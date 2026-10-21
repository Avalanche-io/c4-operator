"""KatanaHost follows a USD stage into what it reads in turn."""

import os

import fakekatana as fk
import pytest
from c4submit import usd as usdlib
from c4submit.collect import gather
from c4submit.testing import write
from katana_scenes import ASSET, light_project

MARSH = '''#usda 1.0
(
    subLayers = [@./marsh_layout.usda@]
)
def Xform "marsh" (
    references = @reeds/reeds.usd@
)
{
    asset mud = @tex/mud.<UDIM>.tex@
}
'''


@pytest.fixture(autouse=True)
def _no_pxr(monkeypatch):
    # As in a Katana without USD's Python: the text layers are read here.
    monkeypatch.setattr(usdlib, "_with_pxr", lambda path: None)


def marsh_files(show):
    marsh = os.path.join(show, "assets", "marsh")
    write(os.path.join(marsh, "marsh_set.usda"), MARSH.encode())
    write(os.path.join(marsh, "marsh_layout.usda"), b'#usda 1.0\ndef "rocks" { asset a = @rocks.abc@ }\n')
    write(os.path.join(marsh, "rocks.abc"), b"abc")
    write(os.path.join(marsh, "reeds", "reeds.usd"), b"PXR-USDC binary")
    for tile in (1001, 1002):
        write(os.path.join(marsh, "tex", f"mud.{tile}.tex"), b"mud %d" % tile)
    return marsh


def test_a_usdin_stage_brings_what_it_reads_tagged_as_the_node(tmp_path):
    host, _, _ = light_project(tmp_path)
    show = str(tmp_path)
    marsh = marsh_files(show)
    write(os.path.join(show, "fx", "fog", "fog.usd"), b'#usda 1.0\ndef Volume "fog" { asset f = @vdb/fog.$F4.vdb@ }\n')
    deps = host.dependencies()
    via = {os.path.relpath(d.path, show).replace(os.sep, "/"): d for d in deps if " > " in d.node}
    assert set(via) == {"assets/marsh/marsh_layout.usda", "assets/marsh/rocks.abc", "assets/marsh/reeds/reeds.usd",
                        "assets/marsh/tex/mud.<UDIM>.tex", "fx/fog/vdb/fog.$F4.vdb"}
    assert via["assets/marsh/rocks.abc"].node == "marsh_usd.fileName > marsh_set.usda"
    assert via["assets/marsh/rocks.abc"].renderables is None, "both Render nodes read the marsh, so all it reads"
    assert via["fx/fog/vdb/fog.$F4.vdb"].renderables == ["Render_beauty"], "as fog_usd: only Render_beauty"
    assert host.warnings() == [
        os.path.join(marsh, "reeds", "reeds.usd") + " is binary USD and USD's Python is not available here, "
        "so what it reads in turn was not collected"]


def test_unticking_the_render_node_leaves_out_what_its_stage_reads(tmp_path):
    host, project, _ = light_project(tmp_path)
    show = str(tmp_path)
    write(os.path.join(show, "fx", "fog", "fog.usd"), b'#usda 1.0\ndef "fog" { asset f = @fog_density.vdb@ }\n')
    write(os.path.join(show, "fx", "fog", "fog_density.vdb"), b"vdb")
    shadow_only = [d for d in host.dependencies() if d.renderables is None or "Render_shadow" in d.renderables]
    assert not any(d.path.endswith("fog_density.vdb") for d in shadow_only)
    col = gather(show, project, [d for d in host.dependencies() if d.renderables is None
                                 or "Render_beauty" in d.renderables])
    assert "fx/fog/fog_density.vdb" in col.files


def test_no_usd_no_warnings_and_a_missing_stage_reads_nothing(tmp_path):
    host, _, _ = light_project(tmp_path)
    fk.node("UsdIn", "gone_usd", fileName=(str(tmp_path / "assets" / "gone.usda"), ASSET))
    deps = host.dependencies()
    assert not [d for d in deps if " > " in d.node], "the fixture's stages read nothing in turn; a missing one neither"
    assert host.warnings() == []


def test_the_warning_is_shown_with_the_check(tmp_path):
    from c4submit.submitter import Draft, prepare
    from c4submit.tool import Tool
    from katana_scenes import KATANA_RENDER

    host, _, _ = light_project(tmp_path)
    marsh_files(str(tmp_path))
    prepared = prepare(Draft(host, Tool(KATANA_RENDER), {"name": "saltmarsh", "root": str(tmp_path)}))
    assert any("reeds.usd is binary USD" in p.message for p in prepared.problems if p.level == "warning")
