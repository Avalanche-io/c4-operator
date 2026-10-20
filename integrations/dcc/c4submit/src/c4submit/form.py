"""The submit form a tool's parameters draw, without a toolkit (TOOL.md
section 6). A panel draws each field in its own widgets (Qt, Houdini
parameters, Nuke knobs) and reads and writes values through this model,
so every DCC shows the same fields, defaults, links and errors.

- ``show: artist`` fields are drawn; ``advanced`` ones are drawn folded;
  ``wrangler`` and ``hidden`` are not drawn in a DCC (the HUD shows
  ``wrangler``) and keep their defaults.
- A field with ``from`` is derived from the scene and shows a link toggle:
  linked, it follows the scene; typed into, it is unlinked and keeps what
  was typed until relinked.
- The submission's ``with`` holds only what differs from the tool's
  defaults, plus every derived value, so a saved file stays small.
"""

from typing import Any, Callable, Dict, List, Optional, Tuple

from c4submit.tool import Param, Tool

DRAWN = ("artist", "advanced")

# Where a field's derived value comes from: a function of the parameter's
# ``from`` that returns a value, or None when the scene has none.
Derive = Callable[[str], Any]


class Field:
    def __init__(self, param: Param):
        self.param = param
        self.derived: Any = None
        self.linked = param.source is not None
        self._typed: Any = None
        self.touched = False

    @property
    def name(self) -> str:
        return self.param.name

    @property
    def label(self) -> str:
        return self.param.label

    @property
    def control(self) -> str:
        return self.param.control

    @property
    def advanced(self) -> bool:
        return self.param.show == "advanced"

    @property
    def derivable(self) -> bool:
        return self.param.source is not None

    @property
    def value(self) -> Any:
        if self.linked and self.derived is not None:
            return self.derived
        if self.touched:
            return self._typed
        return self.param.default

    @property
    def error(self) -> str:
        return self.param.check(self.value)

    def __repr__(self) -> str:
        return f"Field({self.name}={self.value!r}{', linked' if self.linked and self.derived is not None else ''})"


class Form:
    def __init__(self, tool: Tool, derive: Optional[Derive] = None, values: Optional[Dict[str, Any]] = None,
                 drawn: Tuple[str, ...] = DRAWN):
        self.tool = tool
        self.fields: List[Field] = [Field(p) for p in tool.params]
        self._drawn = drawn
        if derive:
            self.refresh(derive)
        for k, v in (values or {}).items():
            self.set(k, v)

    def field(self, name: str) -> Field:
        for f in self.fields:
            if f.name == name:
                return f
        raise KeyError(f"{self.tool.name} has no parameter {name!r}")

    def drawn(self) -> List[Field]:
        """The fields a panel draws, in the tool's order."""
        return [f for f in self.fields if f.param.show in self._drawn]

    def groups(self) -> List[Tuple[str, List[Field]]]:
        """The drawn fields by ``groupLabel``, in first-seen order, artist
        fields first; advanced fields come last under ``Advanced`` unless
        they carry a group of their own."""
        order: List[str] = []
        by: Dict[str, List[Field]] = {}
        for f in sorted(self.drawn(), key=lambda f: f.advanced):
            g = f.param.group or ("Advanced" if f.advanced else "")
            if g not in by:
                order.append(g)
                by[g] = []
            by[g].append(f)
        return [(g, by[g]) for g in order]

    def refresh(self, derive: Derive) -> None:
        """Re-read every derived value from the scene."""
        for f in self.fields:
            if f.param.source:
                f.derived = derive(f.param.source)

    def set(self, name: str, value: Any) -> str:
        """Types a value into a field, unlinking it; returns its error."""
        f = self.field(name)
        f._typed, f.touched, f.linked = value, True, False
        return f.error

    def link(self, name: str, linked: bool = True) -> None:
        """Turns a derived field's link on (follow the scene) or off (keep
        the value it shows now)."""
        f = self.field(name)
        if not f.derivable:
            return
        if not linked and f.linked:
            f._typed, f.touched = f.value, True
        f.linked = linked

    def errors(self) -> List[Tuple[str, str]]:
        """(parameter, problem) for every field whose value is wrong,
        drawn or not."""
        return [(f.name, f.error) for f in self.fields if f.error]

    def values(self, override: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        """The ``with`` of a job: derived values, and values that differ
        from the tool's defaults, typed as the parameters say. ``override``
        replaces a field's value for one job (a renderable's own)."""
        out: Dict[str, Any] = {}
        for f in self.fields:
            v = f.value
            derived = f.linked and f.derived is not None
            if override and f.name in override:
                v, derived = override[f.name], True
            if v is None or v == "" or v == []:
                continue
            try:
                v = f.param.coerce(v)
            except ValueError:
                pass  # errors() reports it; send what was typed
            if derived or not f.param.has_default or v != f.param.default:
                out[f.name] = v
        return out
