"""What a USD file reads in turn: its sublayers, references, payloads and
asset paths (textures, volumes, caches), and theirs, so a host that finds a
USD file in its scene can send what that file needs too.

With USD's own Python (``pxr``, which Houdini, Katana, Maya with its USD
plug-in and Nuke carry), this is ``UsdUtils.ComputeAllDependencies``,
which resolves exactly as a renderer would. Without it, text layers
(``.usda``) are read here: every ``@...@`` asset path, relative to the
layer that names it, followed into further text layers. A binary layer
(``.usdc``, or ``.usd`` holding one) cannot be read without ``pxr``; it is
sent, and named in ``unread`` so the host can say so.
"""

import os
import re
from typing import List, Optional, Tuple

from c4submit import paths
from c4submit.host import Dependency

_ASSET = re.compile(r"@@@(.+?)@@@|@([^@\n]+)@")
_LAYER = (".usd", ".usda", ".usdc")
# A path with a scheme (op:, anon:, http:) is not a file; a drive letter is.
_SCHEME = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]+:")


def _is_binary(path: str) -> bool:
    try:
        with open(path, "rb") as f:
            return f.read(8) == b"PXR-USDC"
    except OSError:
        return False


def _with_pxr(path: str) -> Optional[List[str]]:
    try:
        from pxr import Sdf, UsdUtils  # type: ignore
    except ImportError:
        return None
    layers, assets, unresolved = UsdUtils.ComputeAllDependencies(Sdf.AssetPath(path))
    out = [getattr(layer, "realPath", "") or layer.identifier for layer in layers
           if not getattr(layer, "anonymous", False)]
    out += [str(a) for a in assets]
    anchor = os.path.dirname(path)
    out += [u if paths.is_absolute(u) or _SCHEME.match(u) else os.path.join(anchor, u)
            for u in (str(u) for u in unresolved)]
    return out


def _from_text(path: str, unread: List[str]) -> List[str]:
    out, todo, seen = [], [path], set()
    while todo:
        layer = todo.pop()
        key = paths.norm(layer)
        if key in seen:
            continue
        seen.add(key)
        if layer != path:
            out.append(layer)
        if not os.path.isfile(layer):
            continue
        if _is_binary(layer):
            unread.append(layer)
            continue
        try:
            with open(layer, encoding="utf-8", errors="replace") as f:
                text = f.read()
        except OSError:
            continue
        anchor = os.path.dirname(layer)
        for m in _ASSET.finditer(text):
            ref = (m.group(1) or m.group(2)).strip()
            if not ref or _SCHEME.match(ref) and not paths.is_windows(ref):
                continue
            full = ref if paths.is_absolute(ref) else os.path.join(anchor, ref)
            if full.lower().endswith(_LAYER):
                todo.append(full)
            elif paths.norm(full) not in seen:
                seen.add(paths.norm(full))
                out.append(full)
    return out


def dependencies(path: str, node: str = "", use_pxr: bool = True) -> Tuple[List[Dependency], List[str]]:
    """The files a USD file reads, as dependencies (the file itself is not
    among them), and the binary layers that could not be read without
    ``pxr``. Paths keep their ``<UDIM>`` and frame tokens, so c4submit
    expands them as sequences and tiles."""
    unread: List[str] = []
    found = _with_pxr(path) if use_pxr else None
    if found is None:
        found = _from_text(path, unread)
    out, seen = [], {paths.norm(path)}
    for p in found:
        if _SCHEME.match(p) and not paths.is_windows(p):
            continue
        n = paths.norm(p)
        if n in seen:
            continue
        seen.add(n)
        out.append(Dependency(path=n, node=node or os.path.basename(path)))
    return out, unread
