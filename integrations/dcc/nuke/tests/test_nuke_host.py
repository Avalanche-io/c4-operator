"""NukeHost: what the open script tells c4submit."""

import os

import fakenuke as nk
from c4submit.testing import write
from nuke_scripts import comp_script

from c4_nuke.host import NukeHost, ocio_search_dirs


def by_name(items):
    return {r.name: r for r in items}


def test_writes_become_renderables_with_their_ranges(tmp_path):
    host, n = comp_script(tmp_path)
    rs = by_name(host.renderables())
    assert list(rs) == ["Write_bg", "Write_main"], "the disabled Write_old is left out"
    assert rs["Write_bg"].frames is None, "without its own limit a Write follows the script's range"
    assert rs["Write_main"].frames == "1001-1048"
    assert rs["Write_main"].outputs == [os.path.join(str(tmp_path), "comp", "out", "main.####.exr")]
    assert host.frames() == "1001-1048"
    assert host.scene_file().endswith("sm0140_comp_v004.nk")


def test_a_write_reading_another_writes_render_is_after_it(tmp_path):
    host, _ = comp_script(tmp_path)
    rs = by_name(host.renderables())
    assert rs["Write_main"].reads == ["Write_bg"], "Read_bg's bg.#### is Write_bg's bg.%04d"
    assert rs["Write_bg"].reads == []
    assert rs["Write_main"].note == "after Write_bg"


def test_disabled_writes_are_left_out(tmp_path):
    host, n = comp_script(tmp_path)
    n["Write_bg"]["disable"].setValue(True)
    rs = by_name(host.renderables())
    assert list(rs) == ["Write_main"]
    assert rs["Write_main"].reads == [], "a disabled Write renders nothing for another to wait on"


def test_dependencies_carry_the_writes_that_need_them(tmp_path):
    host, _ = comp_script(tmp_path)
    deps = {d.node: d for d in host.dependencies()}
    assert set(deps) == {"Read_plate", "Read_beauty", "Read_bg", "OCIOFileTransform1"}
    assert deps["Read_plate"].renderables == ["Write_bg"]
    assert deps["Read_plate"].frames == "1001-1048"
    assert deps["Read_bg"].renderables == ["Write_main"]
    assert deps["OCIOFileTransform1"].renderables == ["Write_main"]
    assert deps["OCIOFileTransform1"].frames is None, "a single file has no frames"
    assert not any(d.optional for d in deps.values())


def test_writes_inside_groups_and_reads_through_groups(tmp_path):
    host, n = comp_script(tmp_path)
    show = str(tmp_path)
    # Group1 takes Read_bg in; inside, its Input feeds a Grade and a Write.
    g = nk.group("Group1", n["Read_bg"])
    inp = nk.node("Input", "Input1", parent=g, number=0)
    gr = nk.node("Grade", "Grade2", inp, parent=g)
    nk.write("Write_g", os.path.join(show, "comp", "out", "g.####.exr"), gr, parent=g)
    out = nk.node("Output", "Output1", gr, parent=g)
    assert out.fullName() == "Group1.Output1"
    # A Read inside a second group, whose Output feeds a top-level Write.
    g2 = nk.group("Group2")
    rd = nk.read("Read_in_group", os.path.join(show, "plates", "sm0140.####.exr"), parent=g2)
    nk.node("Output", "Output1", rd, parent=g2)
    nk.write("Write_top", os.path.join(show, "comp", "out", "top.####.exr"), g2)

    rs = by_name(host.renderables())
    assert "Group1.Write_g" in rs
    assert rs["Group1.Write_g"].reads == ["Write_bg"], "through the group's Input to Read_bg"
    deps = {d.node: d for d in host.dependencies()}
    assert deps["Group2.Read_in_group"].renderables == ["Write_top"], "through the group's Output"
    assert sorted(deps["Read_bg"].renderables) == ["Group1.Write_g", "Write_main"]


def test_a_write_that_reads_its_file_back(tmp_path):
    host, n = comp_script(tmp_path)
    show = str(tmp_path)
    n["Write_bg"]["reading"].setValue(True)
    nk.write("Write_over", os.path.join(show, "comp", "out", "over.####.exr"), nk.node("Grade", "Grade9", n["Write_bg"]))
    rs = by_name(host.renderables())
    assert rs["Write_over"].reads == ["Write_bg"]
    deps = [d for d in host.dependencies() if d.node == "Write_bg"]
    assert len(deps) == 1 and deps[0].renderables == ["Write_over"]
    assert not any(d.node == "Read_plate" and "Write_over" in d.renderables for d in host.dependencies()), \
        "past a Write reading its file, its inputs are not read"


def test_files_of_other_nodes_cameras_and_proxies(tmp_path):
    host, n = comp_script(tmp_path)
    show = str(tmp_path)
    abc = write(os.path.join(show, "cam", "sm0140.abc"))
    cam_off = nk.node("Camera2", "Camera_off", read_from_file=False, file=os.path.join(show, "cam", "stale.abc"))
    cam_on = nk.node("Camera2", "Camera_on", read_from_file=True, file=abc)
    vf = nk.node("Vectorfield", "Vectorfield1", n["Grade1"], vfield_file=os.path.join(show, "luts", "show.cube"))
    scan = nk.node("ScanlineRender", "Scan1", vf, cam_off, cam_on)
    n["Write_main"].setInput(0, scan)
    n["Read_plate"].add("proxy", os.path.join(show, "proxy", "sm0140.####.jpg"))
    nk.read("Read_dangling", os.path.join(show, "plates", "old.####.exr"))
    nk.read("Read_off", os.path.join(show, "plates", "off.####.exr"), disable=True)

    deps = {}
    for d in host.dependencies():
        deps.setdefault(d.node, []).append(d)
    assert "Camera_off" not in deps, "a camera not reading from file reads nothing"
    assert deps["Camera_on"][0].path == abc and deps["Camera_on"][0].renderables == ["Write_main"]
    assert deps["Vectorfield1"][0].renderables == ["Write_main"]
    proxy = [d for d in deps["Read_plate"] if "proxy" in d.path]
    assert proxy and proxy[0].optional, "proxy files are read only in proxy mode"
    assert deps["Read_dangling"][0].renderables == [], "a Read feeding no Write is needed by none"
    assert "Read_off" not in deps


def test_relative_paths_use_the_project_directory(tmp_path):
    host, n = comp_script(tmp_path)
    n["Read_plate"]["file"].setValue("plates/sm0140.####.exr")
    nk.root()["project_directory"].setValue(str(tmp_path))
    d = [d for d in host.dependencies() if d.node == "Read_plate"][0]
    assert d.path == os.path.join(str(tmp_path), "plates", "sm0140.####.exr")
    nk.root()["project_directory"].setValue("")
    d = [d for d in host.dependencies() if d.node == "Read_plate"][0]
    assert d.path == os.path.join(str(tmp_path), "comp", "plates", "sm0140.####.exr"), "else the script's folder"


def test_stereo_paths_expand_per_view(tmp_path):
    host, n = comp_script(tmp_path)
    nk.state().views = ["left", "right"]
    n["Read_plate"]["file"].setValue(os.path.join(str(tmp_path), "plates", "%V", "sm0140_%v.####.exr"))
    paths = sorted(d.path for d in host.dependencies() if d.node == "Read_plate")
    assert paths == [os.path.join(str(tmp_path), "plates", "left", "sm0140_l.####.exr"),
                     os.path.join(str(tmp_path), "plates", "right", "sm0140_r.####.exr")]


def test_custom_ocio_config_and_its_luts(tmp_path):
    host, _ = comp_script(tmp_path)
    cfg = write(os.path.join(str(tmp_path), "color", "config.ocio"), b"ocio_profile_version: 1\nsearch_path: luts:more\n")
    lut = write(os.path.join(str(tmp_path), "color", "luts", "a.spi1d"))
    write(os.path.join(str(tmp_path), "color", "more", "deep", "b.cube"))
    assert len(ocio_search_dirs(cfg)) == 2
    nk.root()["customOCIOConfigPath"].setValue(cfg)
    assert not [d for d in host.dependencies() if d.path == cfg], "only when the config is custom"
    nk.root()["OCIO_config"].setValue("custom")
    color = [d for d in host.dependencies() if d.node == "Project Settings > Color"]
    assert [d.path for d in color][:2] == [cfg, lut]
    assert len(color) == 3 and all(d.renderables is None for d in color), "every job reads the colour config"


def test_unsaved_modified_and_save(tmp_path):
    nk.reset("")
    host = NukeHost(env={})
    assert host.scene_file() == ""
    _, _ = comp_script(tmp_path, modified=True)
    host = NukeHost(env={})
    assert host.is_modified()
    host.save()
    assert not host.is_modified() and nk.state().saves == 1


def test_selection_is_the_selected_reads_files(tmp_path):
    host, n = comp_script(tmp_path)
    n["Read_plate"].selected = True
    n["Grade1"].selected = True
    assert host.selection() == [os.path.join(str(tmp_path), "plates", "sm0140.####.exr")]
