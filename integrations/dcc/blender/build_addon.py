"""Builds the C4 Farm add-on as one zip for Blender's "Install from Disk":
the add-on folder, with a copy of c4submit inside it
(``c4_blender/vendor/c4submit``), which the add-on imports first.

    python integrations/dcc/blender/build_addon.py [-o FILE]

writes ``integrations/dcc/blender/dist/c4_blender-<version>.zip`` unless
``-o`` says where. Standard library only; Blender is not needed.
"""

import argparse
import ast
import os
import sys
import zipfile
from typing import Iterator, List, Optional, Tuple

HERE = os.path.dirname(os.path.abspath(__file__))
ADDON = os.path.join(HERE, "addons", "c4_blender")
C4SUBMIT = os.path.normpath(os.path.join(HERE, "..", "c4submit", "src", "c4submit"))
LICENSE = os.path.normpath(os.path.join(HERE, "..", "LICENSE"))


def version() -> str:
    """The add-on's version, from ``bl_info``, read without importing it."""
    with open(os.path.join(ADDON, "__init__.py"), encoding="utf-8") as f:
        tree = ast.parse(f.read())
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(getattr(t, "id", "") == "bl_info" for t in node.targets):
            return ".".join(str(n) for n in ast.literal_eval(node.value)["version"])
    raise ValueError("the add-on's __init__.py has no bl_info")


def _files(top: str, skip: Tuple[str, ...] = ()) -> Iterator[Tuple[str, str]]:
    """(path, path under top with forward slashes) for each file to ship."""
    for d, dirs, names in os.walk(top):
        dirs[:] = sorted(x for x in dirs if x != "__pycache__" and not x.startswith(".")
                         and not (d == top and x in skip))
        for n in sorted(names):
            if n.endswith((".pyc", ".pyo")) or n.startswith("."):
                continue
            p = os.path.join(d, n)
            yield p, os.path.relpath(p, top).replace(os.sep, "/")


def build(out: Optional[str] = None) -> str:
    """Writes the zip and returns its path."""
    out = out or os.path.join(HERE, "dist", f"c4_blender-{version()}.zip")
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        for p, rel in _files(ADDON, skip=("vendor",)):
            z.write(p, "c4_blender/" + rel)
        for p, rel in _files(C4SUBMIT):
            z.write(p, "c4_blender/vendor/c4submit/" + rel)
        if os.path.isfile(LICENSE):
            z.write(LICENSE, "c4_blender/LICENSE")
    return out


def main(argv: Optional[List[str]] = None) -> int:
    ap = argparse.ArgumentParser(description="Build the C4 Farm add-on zip for Blender's Install from Disk.")
    ap.add_argument("-o", "--out", help="where to write the zip (default dist/c4_blender-<version>.zip)")
    args = ap.parse_args(argv)
    print(build(args.out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
