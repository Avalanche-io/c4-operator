"""Render one Movie Render Queue job of an Unreal project over this unit's
frames, as the farm runs it:

    python unreal_render.py PROJECT.uproject map=MAP sequence=SEQ config=MANIFEST

It is run by Unreal's own Python (``Engine/Binaries/ThirdParty/Python3/...``,
the tool's ``runs.exe``), so the same command works in a Linux image and on
a Windows workstation, and it finds ``UnrealEditor-Cmd`` beside it (or at
``UNREAL_EDITOR``). Standard library only, Python 3.9 and up.

``config`` is the job's queue manifest (``C4/queue/<nn>-<job>.utxt``, from
the project's folder), which the Unreal executor writes for every
submission. The script copies it with two changes and gives the copy to
Unreal with ``-MoviePipelineConfig``:

- the job's frames are this unit's (Output settings, custom playback range);
- its output folder is ``render/<job>/`` at the top of the job's tree, so the
  frames land where the farm keeps them. The file names stay the job's own.

Movie Render Queue renders a job's range in one process, so the tool's
units are whole jobs (``frames: {chunk: all}``). While Unreal runs, the
script watches the output folder: a frame is done once a later frame's file
is there, or Unreal has exited, and each is a ``c4_frame`` line with
``openjd_progress``. Unreal's log passes through, and its own progress
lines, where they can be read, are ``openjd_progress`` too.

The parsing and patching are plain functions, tested without Unreal.
"""

import os
import re
import subprocess
import sys
import threading
from typing import Dict, Iterable, List, Mapping, Optional, Sequence, Set, Tuple

#: The arguments the script takes after the project.
KEYS = ("map", "sequence", "config")
#: Where the engine's own new-process executor puts a manifest, under the
#: project's Saved folder; -MoviePipelineConfig is given it from Saved.
MANIFEST_DIR = "MovieRenderPipeline/C4"
#: The startup map and game mode the engine's own executor uses; the job's
#: map comes from the manifest.
ENTRY_MAP = "MoviePipelineEntryMap?game=/Script/MovieRenderPipelineCore.MoviePipelineGameMode"
#: Image files Movie Render Queue writes, one per frame (per pass).
IMAGE_EXTS = (".exr", ".png", ".jpg", ".jpeg", ".bmp", ".tif", ".tiff")
#: How often the output folder is looked at while Unreal runs, in seconds.
POLL = 2.0


class Refusal(Exception):
    """A failure the script can name: ``kind`` is ``permanent`` or ``retry``."""

    def __init__(self, kind: str, reason: str):
        super().__init__(reason)
        self.kind, self.reason = kind, reason

    def line(self) -> str:
        return f"c4_fail: {self.kind} {self.reason}"


# --- arguments ------------------------------------------------------------------

def parse_args(argv: Sequence[str]) -> Dict[str, str]:
    """The project and the ``key=value`` arguments after it."""
    if not argv:
        raise Refusal("permanent", "unreal_render.py needs the project, then map=, sequence= and config=")
    out = {"project": argv[0], **{k: "" for k in KEYS}}
    for a in argv[1:]:
        k, eq, v = a.partition("=")
        if not eq or k not in KEYS:
            raise Refusal("permanent", f"unreal_render.py takes {', '.join(k + '=' for k in KEYS)}, not {a!r}")
        out[k] = v
    if not out["project"].lower().endswith(".uproject"):
        raise Refusal("permanent", f"{os.path.basename(out['project'])} is not an Unreal project (.uproject)")
    if not out["config"]:
        raise Refusal("permanent", "no config: the job's queue manifest, which the Unreal executor writes")
    return out


def unit_frames(env: Mapping[str, str]) -> List[int]:
    """The unit's frames, as c4-unit gives them: C4_FRAME_START, C4_FRAME_END
    and C4_FRAME_STEP, or C4_FRAMES (``1001,1004,1010-1012``)."""
    label = env.get("C4_FRAMES", "")
    if "," in label or (label and not env.get("C4_FRAME_START")):
        out: List[int] = []
        for part in label.split(","):
            span, _, step = part.strip().partition(":")
            a, _, b = span.partition("-")
            out += list(range(int(a), int(b or a) + 1, int(step or 1)))
        return out
    start = env.get("C4_FRAME_START", "")
    if not start:
        raise Refusal("permanent", "the unit has no frames (C4_FRAME_START is not set)")
    end = env.get("C4_FRAME_END") or start
    return list(range(int(start), int(end) + 1, int(env.get("C4_FRAME_STEP") or "1")))


def mrq_range(frames: Sequence[int]) -> Tuple[int, int, int]:
    """(start, end, step) as Movie Render Queue's Output settings take
    them: the end is one past the last frame. Movie Render Queue renders
    one range, every frame or every Nth."""
    fs = sorted(set(frames))
    if not fs:
        raise Refusal("permanent", "the unit has no frames")
    step = fs[1] - fs[0] if len(fs) > 1 else 1
    if any(b - a != step for a, b in zip(fs, fs[1:])):
        raise Refusal("permanent", f"Movie Render Queue renders one range of frames, not {fs[0]}...{fs[-1]} with gaps; "
                                   "submit each range as its own job")
    return fs[0], fs[-1] + 1, step


def resolve_config(config: str, project: str) -> str:
    """The manifest's path: from the project's folder unless absolute."""
    p = config if os.path.isabs(config) else os.path.join(os.path.dirname(project), *config.split("/"))
    return os.path.normpath(p)


def job_name(config: str) -> str:
    """The job's folder under render/: the manifest's name without the
    queue position the executor puts first (``01-sh010-lighting.utxt`` is
    ``sh010-lighting``)."""
    stem = os.path.splitext(os.path.basename(config))[0]
    return re.sub(r"^\d+-", "", stem) or stem


def editor_for(python: str, platform: str = sys.platform) -> str:
    """UnrealEditor-Cmd beside Unreal's own Python: from
    ``Engine/Binaries/ThirdParty/Python3/<platform>/...`` to
    ``Engine/Binaries/<platform>/UnrealEditor-Cmd``."""
    parts = re.split(r"[\\/]", python)
    try:
        i = len(parts) - 1 - parts[::-1].index("ThirdParty")
    except ValueError:
        raise Refusal("permanent", f"{python} is not Unreal's own Python; run the tool with it, "
                                   "or set UNREAL_EDITOR to UnrealEditor-Cmd") from None
    sep = "\\" if "\\" in python and "/" not in python else "/"
    folder = {"win32": "Win64", "darwin": "Mac"}.get(platform, "Linux")
    exe = "UnrealEditor-Cmd.exe" if platform == "win32" else "UnrealEditor-Cmd"
    return sep.join(parts[:i] + [folder, exe])


def command(editor: str, project: str, manifest: str) -> List[str]:
    """Unreal's command line for one job's manifest, as the engine's own
    new-process executor starts a render, offscreen and unattended.
    ``manifest`` is from the project's Saved folder."""
    return [editor, project, ENTRY_MAP, "-game", f'-MoviePipelineConfig={manifest}', "-windowed",
            "-ResX=1280", "-ResY=720", "-RenderOffscreen", "-Unattended", "-NoLoadingScreen", "-FixedSeed",
            "-Multiprocess", "-nohmd", "-log", "-StdOut", "-allowStdOutLogVerbosity"]


# --- the manifest ---------------------------------------------------------------------

_BEGIN = re.compile(r'^(\s*)Begin Object\b(.*)$')
_END = re.compile(r'^\s*End Object\b')
_CLASS = re.compile(r'\bClass=(\S+)')
_NAME = re.compile(r'\bName="?([^"\s]+)"?')
_PROP = re.compile(r'^\s*([A-Za-z_][A-Za-z0-9_]*)(\(\d+\))?=')


def _blocks(lines: List[str]) -> List[Tuple[str, str, int, int, str]]:
    """Each object block: (class or "", name, first line, End line, indent)."""
    out, stack = [], []
    for i, line in enumerate(lines):
        m = _BEGIN.match(line)
        if m:
            c, n = _CLASS.search(m.group(2)), _NAME.search(m.group(2))
            stack.append((c.group(1) if c else "", n.group(1) if n else "", i, m.group(1)))
        elif _END.match(line) and stack:
            c, n, start, indent = stack.pop()
            out.append((c, n, start, i, indent))
    return out


def set_output(text: str, start: int, end: int, step: int, folder: str) -> str:
    """The manifest with its Output settings rendering ``start`` to before
    ``end`` (every ``step``th frame) into ``folder``. The settings are the
    object of class MoviePipelineOutputSetting; its properties are set in
    the last block of that name (the one that holds them, after the
    declaration), and added there when the job left them at their defaults."""
    lines = text.splitlines()
    blocks = _blocks(lines)
    names = {n for c, n, *_ in blocks if c.rsplit(".", 1)[-1] == "MoviePipelineOutputSetting"}
    if not names:
        raise Refusal("permanent", "the job's config has no Output settings (MoviePipelineOutputSetting); "
                                   "the farm renders jobs whose config is a Movie Render Queue preset, "
                                   "not a Movie Render Graph")
    name = sorted(names)[0]
    _, _, first, last, indent = [b for b in blocks if b[1] == name][-1]
    folder = folder.replace("\\", "/").rstrip("/") + "/"
    want = {"bUseCustomPlaybackRange": "True", "CustomStartFrame": str(start), "CustomEndFrame": str(end),
            "OutputFrameStep": str(step), "OutputDirectory": f'(Path="{folder}")'}
    done: Set[str] = set()
    for j in _own_lines(lines, first, last):
        m = _PROP.match(lines[j])
        if m and m.group(1) in want and not m.group(2):
            pad = lines[j][:len(lines[j]) - len(lines[j].lstrip())]
            lines[j] = f"{pad}{m.group(1)}={want[m.group(1)]}"
            done.add(m.group(1))
    pad = indent + "   "
    add = [f"{pad}{k}={v}" for k, v in want.items() if k not in done]
    lines[last:last] = add
    return "\n".join(lines) + ("\n" if text.endswith("\n") else "")


def _own_lines(lines: List[str], first: int, last: int) -> List[int]:
    """The lines of the block from first to last that are its own, not in
    a block nested in it."""
    out, depth = [], 0
    for j in range(first + 1, last):
        if _BEGIN.match(lines[j]):
            depth += 1
        elif _END.match(lines[j]):
            depth -= 1
        elif depth == 0:
            out.append(j)
    return out


def renders_map(text: str, map_pkg: str) -> bool:
    """Whether the manifest's job is on this map (``/Game/Maps/Sh010``)."""
    return not map_pkg or re.search(re.escape(map_pkg) + r'(?:\.|"|\'|\)|$)', text, re.M) is not None


# --- what Unreal says and writes ---------------------------------------------------------

#: The last digits of a name that stand apart from a word (``.1001``,
#: ``_0042``), not the end of one (``sh010``).
_FRAME_OF = re.compile(r"(?:^|[^A-Za-z0-9])(\d+)(?!.*\d)")
#: Movie Render Queue's progress, where its log gives one: a percentage on a
#: LogMovieRenderPipeline line, or "frame N/M" (N of M).
_PCT = re.compile(r"LogMovieRenderPipeline.*?(\d+(?:\.\d+)?)\s?%")
_N_OF_M = re.compile(r"LogMovieRenderPipeline.*?\bframe\b\D{0,12}?(\d+)\s*(?:/|of)\s*(\d+)", re.I)


def progress_of(line: str) -> Optional[float]:
    """A percentage from one of Unreal's log lines, or None."""
    m = _PCT.search(line)
    if m:
        return min(float(m.group(1)), 100.0)
    m = _N_OF_M.search(line)
    if m and int(m.group(2)) > 0:
        return min(100.0 * int(m.group(1)) / int(m.group(2)), 100.0)
    return None


def frame_of(name: str) -> Optional[int]:
    """The frame number of an output file's name: its last digits before
    the extension (``sh010_seq.FinalImage.1001.exr`` is 1001)."""
    stem, ext = os.path.splitext(os.path.basename(name))
    if ext.lower() not in IMAGE_EXTS:
        return None
    m = _FRAME_OF.search(stem)
    return int(m.group(1)) if m else None


def on_disk(folder: str) -> Dict[int, List[str]]:
    """The frames under the output folder, each with its files."""
    out: Dict[int, List[str]] = {}
    for d, _, files in os.walk(folder):
        for f in files:
            n = frame_of(f)
            if n is not None:
                out.setdefault(n, []).append(os.path.join(d, f).replace("\\", "/"))
    return {n: sorted(ps) for n, ps in out.items()}


def finished(found: Mapping[int, List[str]], wanted: Iterable[int], told: Set[int], final: bool) -> List[Tuple[int, str]]:
    """The frames to report now, in order, with one file each: those of the
    unit's frames found on disk and not yet told, once a later frame's file
    is there (Movie Render Queue writes frames in order, so its file is
    whole), or every one once Unreal has exited."""
    want = set(wanted)
    have = sorted(n for n in found if n in want)
    latest = have[-1] if have else None
    return [(n, found[n][0]) for n in have if n not in told and (final or n < latest)]


# --- running it ---------------------------------------------------------------------------

def run(opts: Dict[str, str], env: Mapping[str, str], python: str = sys.executable, out=print) -> int:
    project = os.path.normpath(opts["project"])
    if not os.path.isfile(project):
        raise Refusal("permanent", f"no Unreal project at {project}")
    tree = env.get("C4_TREE") or os.path.dirname(project)
    frames = unit_frames(env)
    start, end, step = mrq_range(frames)
    editor = env.get("UNREAL_EDITOR") or editor_for(python)
    if not os.path.isfile(editor):
        alt = editor.replace("UnrealEditor-Cmd", "UnrealEditor")
        if not os.path.isfile(alt):
            raise Refusal("permanent", f"no UnrealEditor-Cmd at {editor}; set UNREAL_EDITOR to it")
        editor = alt

    config = resolve_config(opts["config"], project)
    if config.lower().endswith(".py"):
        config = build(editor, project, config, env, out)
    if not os.path.isfile(config):
        raise Refusal("permanent", f"no queue manifest at {config}; submit again from Unreal, which writes it")
    with open(config, encoding="utf-8-sig") as f:
        text = f.read()
    if not renders_map(text, opts["map"]):
        raise Refusal("permanent", f"the queue manifest {os.path.basename(config)} does not render {opts['map']}")
    name = job_name(config)
    folder = os.path.join(tree, "render", name)
    os.makedirs(folder, exist_ok=True)
    saved = os.path.join(os.path.dirname(project), "Saved", *MANIFEST_DIR.split("/"))
    os.makedirs(saved, exist_ok=True)
    unit = f"{name}-{start}-{end - 1}.utxt"
    with open(os.path.join(saved, unit), "w", encoding="utf-8") as f:
        f.write(set_output(text, start, end, step, folder))

    out(f"openjd_status: rendering {name}, frames {start}-{end - 1}, with Movie Render Queue", flush=True)
    cmd = command(editor, project, f"{MANIFEST_DIR}/{unit}")
    told: Set[int] = set()
    lock = threading.Lock()
    stop = threading.Event()

    def tell(final: bool) -> None:
        with lock:
            for n, path in finished(on_disk(folder), frames, told, final):
                told.add(n)
                out(f"c4_frame: {n} {path}", flush=True)
                out(f"openjd_progress: {100.0 * len(told) / len(frames):.1f}", flush=True)

    def watch() -> None:
        while not stop.wait(POLL):
            tell(False)

    w = threading.Thread(target=watch, daemon=True)
    w.start()
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, cwd=os.path.dirname(project),
                            env=dict(env), text=True, encoding="utf-8", errors="replace", bufsize=1)
    for line in proc.stdout:
        line = line.rstrip("\r\n")
        pct = progress_of(line)
        with lock:
            out(line, flush=True)
            if pct is not None and not told:
                out(f"openjd_progress: {pct:.1f}", flush=True)
    code = proc.wait()
    stop.set()
    w.join()
    tell(True)
    missing = [n for n in frames if n not in told]
    if code != 0:
        raise Refusal("retry", f"Unreal exited {code} with {len(told)} of {len(frames)} frames written")
    if missing:
        raise Refusal("retry", f"Unreal exited 0 but wrote no file for frame {missing[0]}"
                               + (f" and {len(missing) - 1} more" if len(missing) > 1 else "") + f" under {folder}")
    return 0


def build(editor: str, project: str, script: str, env: Mapping[str, str], out=print) -> str:
    """Runs a .py config in the editor to build the job (the tool's test
    does: a map, a level sequence and its queue manifest), and returns the
    manifest it wrote, beside the script."""
    manifest = os.path.splitext(script)[0] + ".utxt"
    out(f"openjd_status: building the job with {os.path.basename(script)}", flush=True)
    e = dict(env)
    e["C4_UNREAL_MANIFEST"] = manifest
    code = subprocess.call([editor, project, "-run=pythonscript", f"-script={script}", "-Unattended", "-nullrhi",
                            "-log", "-StdOut", "-allowStdOutLogVerbosity"], env=e, cwd=os.path.dirname(project))
    if code != 0 or not os.path.isfile(manifest):
        raise Refusal("retry", f"building the job with {os.path.basename(script)} failed (exit {code})")
    return manifest


def main(argv: Sequence[str]) -> int:
    try:
        return run(parse_args(argv), os.environ)
    except Refusal as e:
        print(e.line(), flush=True)
        return 2 if e.kind == "permanent" else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
