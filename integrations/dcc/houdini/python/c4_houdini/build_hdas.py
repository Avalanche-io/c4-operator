"""Builds ``otls/c4.hda``: the C4 Render LOP (``c4::c4render::1.0``) and the
C4 Submit ROP (``c4::c4submit::1.0``). A TD runs it once, or a build that
has Houdini does:

    hython integrations/dcc/houdini/python/c4_houdini/build_hdas.py [--out PATH]

Each asset holds the parameters every C4 node has (``nodes.static_group``)
and a PythonModule that calls ``c4_houdini``, so the code is updated with
the package and the asset only when its header changes. The Tool folder is
added to each node as spare parameters, drawn from the tool the artist
picks. Until the HDA is built, ``c4_houdini.nodes.install(node)`` (or the
C4 shelf) gives a null LOP or ROP the same interface.
"""

import argparse
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
PACKAGE = os.path.normpath(os.path.join(HERE, "..", ".."))           # integrations/dcc/houdini
for p in (os.path.join(PACKAGE, "python"), os.path.join(PACKAGE, "..", "c4submit", "src")):
    p = os.path.normpath(p)
    if p not in sys.path:
        sys.path.insert(0, p)

PYTHON_MODULE = '''"""The C4 node's code lives in c4_houdini (integrations/dcc/houdini/python),
so updating the package updates every C4 node without rebuilding this asset."""

from c4_houdini import nodes


def changed(kwargs):
    nodes.changed(kwargs)


def menu(kwargs, which):
    return nodes.menu(kwargs["node"], which)
'''

ON_CREATED = '''from c4_houdini import nodes
nodes.created(kwargs["node"])
'''

ON_LOADED = '''# The Tool folder is saved with the node as spare parameters; the farm is
# read again the first time the artist touches the node.
'''

# kind, network, asset name, label, inputs (min, max), icon
ASSETS = (
    ("lop", "/stage", "c4::c4render::1.0", "C4 Render", (1, 1), "LOP_usdrender_rop"),
    ("rop", "/out", "c4::c4submit::1.0", "C4 Submit", (0, 64), "ROP_subnet"),
)


def build(hda_path: str) -> list:
    """Writes both assets into one HDA file; returns their names."""
    import hou
    from c4_houdini import nodes

    os.makedirs(os.path.dirname(hda_path), exist_ok=True)
    made = []
    for kind, network, name, label, (lo, hi), icon in ASSETS:
        parent = hou.node(network)
        sub = parent.createNode("subnet", "c4_build_" + kind)
        if kind == "lop":
            # The stage passes through: the subnet's output is its input.
            outs = [c for c in sub.children() if c.type().name() == "output"]
            out = outs[0] if outs else sub.createNode("output", "output0")
            out.setInput(0, sub.indirectInputs()[0])
        asset = sub.createDigitalAsset(name=name, hda_file_name=hda_path, description=label,
                                       min_num_inputs=lo, max_num_inputs=hi, ignore_external_references=True)
        d = asset.type().definition()
        d.setParmTemplateGroup(nodes.static_group(kind))
        d.addSection("PythonModule", PYTHON_MODULE)
        d.setExtraFileOption("PythonModule/IsPython", True)
        d.addSection("OnCreated", ON_CREATED)
        d.setExtraFileOption("OnCreated/IsPython", True)
        d.addSection("OnLoaded", ON_LOADED)
        d.setExtraFileOption("OnLoaded/IsPython", True)
        d.setIcon(icon)
        d.save(hda_path)
        asset.destroy()
        made.append(name)
    return made


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Build otls/c4.hda (run with hython).")
    ap.add_argument("--out", default=os.path.join(PACKAGE, "otls", "c4.hda"), help="the HDA file to write")
    args = ap.parse_args(argv)
    for name in build(os.path.abspath(args.out)):
        print(f"built {name} in {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
