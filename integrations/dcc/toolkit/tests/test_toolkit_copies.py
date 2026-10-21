"""Every tool folder that uses c4tool carries the same c4tool.py as
integrations/dcc/toolkit/: a published tool is one folder, so each has its
own copy, and this keeps the copies from drifting."""

import glob
import os
import re

TOOLKIT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DCC = os.path.dirname(TOOLKIT)
SOURCE = os.path.join(TOOLKIT, "c4tool.py")
_IMPORTS = re.compile(r"^\s*(?:import c4tool\b|from c4tool import\b)", re.M)


def tool_folders():
    return sorted(os.path.dirname(y) for y in glob.glob(os.path.join(DCC, "*", "tools", "*", "tool.yaml")))


def uses_c4tool(folder):
    for path in glob.glob(os.path.join(folder, "*.py")):
        if os.path.basename(path) == "c4tool.py":
            continue
        with open(path, encoding="utf-8") as f:
            if _IMPORTS.search(f.read()):
                return True
    return False


def test_each_tool_folder_has_the_sources_c4tool():
    with open(SOURCE, "rb") as f:
        source = f.read()
    folders = tool_folders()
    assert folders, "no tool folders found under integrations/dcc/*/tools/"
    users = [d for d in folders if uses_c4tool(d)]
    assert users, "no tool script imports c4tool"
    stale = []
    for d in folders:
        copy = os.path.join(d, "c4tool.py")
        if not os.path.isfile(copy):
            if d in users:
                stale.append(f"{os.path.relpath(d, DCC)} imports c4tool but has no c4tool.py")
            continue
        with open(copy, "rb") as f:
            if f.read() != source:
                stale.append(f"{os.path.relpath(copy, DCC)} differs from toolkit/c4tool.py")
    fix = "; ".join(f"cp integrations/dcc/toolkit/c4tool.py integrations/dcc/{os.path.relpath(d, DCC)}/"
                    for d in users)
    assert not stale, ("; ".join(stale) + ". Copy the source into each tool folder again: " + fix)
