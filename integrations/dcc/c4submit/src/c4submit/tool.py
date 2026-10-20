"""A published tool as a front end sees it (``c4: tool/1``, TOOL.md): its
name, kind and parameters. Parameters are Open Job Description parameter
definitions, field for field, plus C4's ``show``, ``from`` and
``affectsOutput``; this module reads them and checks values against them.
Nothing a tool says is ever executed here."""

import fnmatch
import os
from typing import Any, Dict, List, Optional

from c4submit import frames as framelib

TYPES = ("STRING", "PATH", "INT", "FLOAT", "BOOL", "RANGE_EXPR")
SHOW = ("artist", "advanced", "wrangler", "hidden")
FROM = ("shot.frames", "scene.frames", "scene.file", "scene.selection", "scene.renderables", "scene.camera", "milestone")

_TRUE = {"true", "yes", "on", "1"}
_FALSE = {"false", "no", "off", "0"}


class Param:
    def __init__(self, d: dict):
        self.raw = d
        self.name: str = d["name"]
        self.type: str = d.get("type", "STRING")
        self.description: str = d.get("description", "")
        self.has_default = "default" in d
        self.default = d.get("default")
        self.allowed: Optional[list] = d.get("allowedValues")
        self.min = d.get("minValue")
        self.max = d.get("maxValue")
        self.min_length = d.get("minLength")
        self.max_length = d.get("maxLength")
        self.object_type = d.get("objectType", "FILE")
        self.data_flow = d.get("dataFlow", "IN")
        self.ui: dict = d.get("userInterface") or {}
        self.show: str = d.get("show", "artist")
        self.source: Optional[str] = d.get("from")
        self.affects_output: bool = d.get("affectsOutput", True)

    @property
    def label(self) -> str:
        return self.ui.get("label") or self.name.replace("_", " ").capitalize()

    @property
    def group(self) -> str:
        return self.ui.get("groupLabel", "")

    @property
    def required(self) -> bool:
        return not self.has_default

    @property
    def item_type(self) -> Optional[str]:
        if self.type.startswith("LIST[") and self.type.endswith("]"):
            return self.type[5:-1]
        return None

    @property
    def control(self) -> str:
        """The control to draw: OpenJD's ``userInterface.control``, or the
        one its type implies."""
        c = self.ui.get("control")
        if c:
            return c
        if self.allowed:
            return "DROPDOWN_LIST"
        if self.type == "BOOL":
            return "CHECK_BOX"
        if self.type in ("INT", "FLOAT"):
            return "SPIN_BOX"
        if self.type == "PATH":
            if self.object_type == "DIRECTORY":
                return "CHOOSE_DIRECTORY"
            return "CHOOSE_OUTPUT_FILE" if self.data_flow == "OUT" else "CHOOSE_INPUT_FILE"
        return "LINE_EDIT"

    @property
    def file_patterns(self) -> List[str]:
        out = []
        for f in self.ui.get("fileFilters") or []:
            out.extend(f.get("patterns") or [])
        return out

    def coerce(self, value: Any) -> Any:
        """The value as the parameter's type; ValueError says what is wrong.
        Text from a line edit is accepted for every type."""
        t = self.item_type
        if t:
            if isinstance(value, str):
                value = [v.strip() for v in value.split(",") if v.strip()]
            if not isinstance(value, (list, tuple)):
                raise ValueError("must be a list")
            item = Param({"name": self.name, "type": t})
            return [item.coerce(v) for v in value]
        if self.type == "INT":
            if isinstance(value, bool):
                raise ValueError("must be a whole number")
            if isinstance(value, float) and value.is_integer():
                return int(value)
            try:
                return int(str(value).strip())
            except ValueError:
                raise ValueError("must be a whole number") from None
        if self.type == "FLOAT":
            if isinstance(value, bool):
                raise ValueError("must be a number")
            try:
                return float(str(value).strip()) if not isinstance(value, (int, float)) else float(value)
            except ValueError:
                raise ValueError("must be a number") from None
        if self.type == "BOOL":
            if isinstance(value, bool):
                return value
            s = str(value).strip().lower()
            if s in _TRUE:
                return True
            if s in _FALSE:
                return False
            raise ValueError("must be on or off")
        if self.type == "RANGE_EXPR":
            s = str(value).strip()
            err = framelib.check(s)
            if err:
                raise ValueError(err)
            return s
        if not isinstance(value, str):
            value = str(value)
        return value

    def check(self, value: Any) -> str:
        """What is wrong with a value, or ''."""
        if value is None or value == "" or value == []:
            return "needs a value" if self.required else ""
        try:
            v = self.coerce(value)
        except ValueError as e:
            return str(e)
        values = v if isinstance(v, list) else [v]
        for x in values:
            if self.allowed is not None and x not in self.allowed:
                return "must be one of " + ", ".join(str(a) for a in self.allowed)
            if isinstance(x, (int, float)) and not isinstance(x, bool):
                if self.min is not None and x < self.min:
                    return f"must be at least {self.min}"
                if self.max is not None and x > self.max:
                    return f"must be at most {self.max}"
            if isinstance(x, str):
                if self.min_length is not None and len(x) < self.min_length:
                    return f"must be at least {self.min_length} characters"
                if self.max_length is not None and len(x) > self.max_length:
                    return f"must be at most {self.max_length} characters"
        return ""


class Tool:
    """A tool's definition. ``d`` is ``tool.yaml`` as JSON, or the farm's
    answer for one tool (``{name, version, id, tool: {...}}``)."""

    def __init__(self, d: dict):
        body = d.get("tool") if isinstance(d.get("tool"), dict) else d
        self.raw = body
        self.name: str = body.get("name") or d.get("name", "")
        self.version: str = str(d.get("version", body.get("version", "")) or "")
        self.id: str = d.get("id", "")
        self.title: str = body.get("title") or self.name
        self.summary: str = body.get("summary", "")
        self.kind: str = body.get("kind", "once")
        self.params: List[Param] = [Param(p) for p in body.get("params") or []]
        self.frames_defaults: dict = body.get("frames") or {}
        needs = body.get("needs") or {}
        self.sizes: Dict[str, dict] = needs.get("sizes") or {}
        self.licences: Dict[str, int] = needs.get("licences") or {}

    def __repr__(self) -> str:
        return f"Tool({self.name}{'@' + self.version if self.version else ''})"

    @property
    def takes_frames(self) -> bool:
        return self.kind == "frames"

    def param(self, name: str) -> Optional[Param]:
        for p in self.params:
            if p.name == name:
                return p
        return None

    def scene_params(self) -> List[Param]:
        return [p for p in self.params if p.source == "scene.file" and p.type == "PATH"]

    def accepts_scene(self, scene_file: str) -> bool:
        """Whether this tool runs scenes like the open one: it has a PATH
        parameter taken from the scene file, whose file filters (if any)
        match the scene's name."""
        base = os.path.basename(scene_file or "")
        for p in self.scene_params():
            pats = p.file_patterns
            if not pats or any(fnmatch.fnmatch(base.lower(), pat.lower()) for pat in pats):
                return True
        return False
