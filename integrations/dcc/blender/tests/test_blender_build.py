"""The zip for Blender's Install from Disk: the add-on with c4submit inside."""

import os
import subprocess
import sys
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

import build_addon  # noqa: E402


def test_the_zip_holds_the_addon_and_c4submit(tmp_path):
    out = build_addon.build(str(tmp_path / "c4_blender.zip"))
    names = zipfile.ZipFile(out).namelist()
    for n in ("c4_blender/__init__.py", "c4_blender/host.py", "c4_blender/panel.py", "c4_blender/LICENSE",
              "c4_blender/vendor/c4submit/__init__.py", "c4_blender/vendor/c4submit/submitter.py"):
        assert n in names
    assert all(n.startswith("c4_blender/") for n in names), "one folder, as Install from Disk expects"
    assert not [n for n in names if "__pycache__" in n or n.endswith(".pyc") or "/tests/" in n]
    assert build_addon.version() == "0.1.0"


def test_the_installed_addon_imports_its_own_c4submit(tmp_path):
    out = build_addon.build(str(tmp_path / "c4_blender.zip"))
    installed = tmp_path / "addons"
    zipfile.ZipFile(out).extractall(str(installed))
    probe = tmp_path / "probe.py"
    probe.write_text(
        "import sys\n"
        f"sys.path[:0] = [{HERE!r}, {str(installed)!r}]\n"
        "import fakebpy\n"
        "fakebpy.install()\n"
        "import c4_blender, c4submit\n"
        "c4_blender.register()\n"
        "print(c4submit.__file__)\n")
    r = subprocess.run([sys.executable, "-I", str(probe)], capture_output=True, text=True, timeout=60)
    assert r.returncode == 0, r.stderr
    assert r.stdout.strip().startswith(str(installed / "c4_blender" / "vendor" / "c4submit"))
