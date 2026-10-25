"""The farms ``c4 farm`` knows, so a panel talks to the farm the artist's
``c4 farm`` does, with no settings of its own.

``c4 farm up`` and ``c4 farm login`` write ``~/.c4/farm.yaml`` (or
``$C4_FARM_CONFIG``)::

    current: studio
    farms:
      studio: {hud: https://c4.petrelfinch.example, tokenFile: ~/.c4/studio.token, project: saltmarsh}
      demo: {hud: http://127.0.0.1:30180, as: wrangler, local: true,
             installerTokenFile: ~/.c4/demo/installer-token}

The farm is picked as ``c4 farm`` picks it: ``$C4_FARM``, else the current
farm, else the only one. ``as`` is a development viewer, which only a
local farm started for development accepts, and only from a request
carrying this machine's installer token (``installerTokenFile``).
"""

import os
import re
from typing import Any, Dict, Optional


def path() -> str:
    return os.environ.get("C4_FARM_CONFIG") or os.path.join(os.path.expanduser("~"), ".c4", "farm.yaml")


def _scalar(s: str) -> Any:
    s = s.strip()
    if len(s) >= 2 and s[0] == s[-1] and s[0] in "'\"":
        return s[1:-1]
    if s in ("true", "false"):
        return s == "true"
    return s


def _flow(s: str) -> Dict[str, Any]:
    out: Dict[str, Any] = {}
    for part in s.strip()[1:-1].split(","):
        if ":" in part:
            k, v = part.split(":", 1)
            out[_scalar(k)] = _scalar(v)
    return out


def parse(text: str) -> Dict[str, Any]:
    """The file's maps, with PyYAML when the application has it, else for
    the shapes ``c4 farm`` writes: nested maps of plain values."""
    try:
        import yaml  # type: ignore
        doc = yaml.safe_load(text)
        return doc if isinstance(doc, dict) else {}
    except ImportError:
        pass
    root: Dict[str, Any] = {}
    stack = [(-1, root)]
    for raw in text.splitlines():
        line = re.sub(r"\s+#.*$", "", raw.rstrip())
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        indent = len(line) - len(line.lstrip())
        key, _, rest = line.strip().partition(":")
        key = _scalar(key)
        while stack and stack[-1][0] >= indent:
            stack.pop()
        parent = stack[-1][1]
        rest = rest.strip()
        if not rest:
            parent[key] = {}
            stack.append((indent, parent[key]))
        elif rest.startswith("{"):
            parent[key] = _flow(rest)
        else:
            parent[key] = _scalar(rest)
    return root


def resolve(name: str = "") -> Optional[Dict[str, Any]]:
    """The farm ``c4 farm`` would talk to, with its ``name``, or None when
    there is no config or no farm in it."""
    try:
        with open(path(), encoding="utf-8") as f:
            cfg = parse(f.read())
    except OSError:
        return None
    farms = cfg.get("farms") or {}
    if not isinstance(farms, dict) or not farms:
        return None
    name = name or os.environ.get("C4_FARM", "") or str(cfg.get("current") or "")
    if not name and len(farms) == 1:
        name = next(iter(farms))
    farm = farms.get(name)
    if not isinstance(farm, dict):
        return None
    out = dict(farm)
    out["name"] = name
    for k in ("tokenFile", "installerTokenFile"):
        v = out.get(k)
        if isinstance(v, str) and v.startswith("~/"):
            out[k] = os.path.join(os.path.expanduser("~"), v[2:])
    return out
