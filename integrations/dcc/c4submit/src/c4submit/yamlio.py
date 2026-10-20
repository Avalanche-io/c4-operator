"""Writing a submission as YAML, and reading one back, with only the
standard library: Nuke, Houdini, Maya and Blender do not all ship PyYAML.

The writer covers what a submission holds (maps, lists, strings, numbers,
booleans and null). Short maps of scalars are written inline, as the
examples in SUBMIT.md are (``with: {stage: sm0140.usda}``). Reading takes
JSON, or YAML when PyYAML is importable.
"""

import json
import re
from typing import Any

_PLAIN = re.compile(r"^[A-Za-z0-9_./][A-Za-z0-9_./ :@+()-]*$")
_RESERVED = {"", "~", "null", "true", "false", "yes", "no", "on", "off", "y", "n"}
_NUMBER = re.compile(r"^[-+]?(\d[\d_]*)?(\.[\d_]*)?([eE][-+]?\d+)?$|^0[xob]|^[-+]?\.(inf|nan)$", re.I)
# Characters a YAML reader refuses or changes raw; written as \u escapes.
_UNSAFE = re.compile("[\x7f-\x9f\u2028\u2029\ufeff\ufffe\uffff\ud800-\udfff]")
_INLINE_WIDTH = 72


def _scalar(v: Any) -> str:
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, float):
        if v != v:
            return ".nan"
        if v in (float("inf"), float("-inf")):
            return ".inf" if v > 0 else "-.inf"
        s = repr(v)
        if "e" in s and "." not in s.split("e")[0]:
            m, e = s.split("e")
            s = f"{m}.0e{e}"  # a YAML 1.1 float needs a dot
        return s
    if isinstance(v, int):
        return str(v)
    s = str(v)
    if (_PLAIN.match(s) and s.lower() not in _RESERVED and not _NUMBER.match(s)
            and not s.endswith(" ") and ": " not in s and " #" not in s and not s.endswith(":")
            and not re.match(r"^[-+]?\d[\d_]*:\d|^\d{4}-\d\d?-\d\d?", s)):  # sexagesimal and dates in YAML 1.1
        return s
    return _quote(s)


def _quote(s: str) -> str:
    return _UNSAFE.sub(lambda m: "\\u%04x" % ord(m.group(0)), json.dumps(s, ensure_ascii=False))


def _key(k: Any) -> str:
    s = str(k)
    return s if _PLAIN.match(s) and ":" not in s and s.lower() not in _RESERVED and not _NUMBER.match(s) else _quote(s)


def _is_scalar(v: Any) -> bool:
    return not isinstance(v, (dict, list, tuple))


def _inline(v: Any) -> str:
    if isinstance(v, dict):
        return "{" + ", ".join(f"{_key(k)}: {_inline(x)}" for k, x in v.items()) + "}"
    if isinstance(v, (list, tuple)):
        return "[" + ", ".join(_inline(x) for x in v) + "]"
    s = _scalar(v)
    # Inside flow collections a plain scalar may not hold these.
    return _quote(str(v)) if not s.startswith('"') and re.search(r"[,\[\]{}]", s) else s


def _flat(v: Any) -> bool:
    """Small enough, and simple enough, to write on one line."""
    if isinstance(v, dict):
        return all(_is_scalar(x) or (isinstance(x, (list, tuple)) and all(_is_scalar(y) for y in x)) for x in v.values())
    if isinstance(v, (list, tuple)):
        return all(_is_scalar(x) for x in v)
    return True


def _emit(v: Any, indent: int, out: list) -> None:
    pad = "  " * indent
    if isinstance(v, dict):
        for k, x in v.items():
            head = f"{pad}{_key(k)}:"
            if _is_scalar(x):
                out.append(f"{head} {_scalar(x)}")
            elif not x:
                out.append(f"{head} {'{}' if isinstance(x, dict) else '[]'}")
            elif _flat(x) and len(head) + len(_inline(x)) + 1 <= _INLINE_WIDTH:
                out.append(f"{head} {_inline(x)}")
            else:
                out.append(head)
                _emit(x, indent + 1, out)
    elif isinstance(v, (list, tuple)):
        for x in v:
            if _is_scalar(x) or (_flat(x) and len(pad) + len(_inline(x)) + 2 <= _INLINE_WIDTH):
                out.append(f"{pad}- {_inline(x) if not _is_scalar(x) else _scalar(x)}")
            else:
                sub: list = []
                _emit(x, indent + 1, sub)
                sub[0] = f"{pad}- " + sub[0].lstrip()
                out.extend(sub)
    else:
        out.append(pad + _scalar(v))


def dump(doc: Any, comments: list = ()) -> str:
    """YAML for a document, with comment lines on top."""
    out = [f"# {c}" if c else "#" for c in comments]
    _emit(doc, 0, out)
    return "\n".join(out) + "\n"


def load(text: str) -> Any:
    """A document from JSON, or from YAML when PyYAML is available."""
    stripped = text.lstrip()
    if stripped.startswith("{"):
        return json.loads(text)
    try:
        import yaml  # type: ignore
    except ImportError:
        raise ValueError("reading a YAML submission needs PyYAML in this application; JSON reads without it") from None
    return yaml.safe_load(text)
