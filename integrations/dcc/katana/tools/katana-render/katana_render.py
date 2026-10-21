"""Render one Render node of a Katana project over this unit's frames.

Python 3 runs it, as the tool's command does::

    python3 katana_render.py PROJECT --node=NODE [--threads=N]

It runs Katana's batch render::

    katana --batch --katana-file=PROJECT --render-node=NODE -t START-END

passes its log through, and works out which frames are finished from the
image files written during the run: each path the log names and each new
image under the job's tree. A frame is reported with ``c4_frame: <frame>
<path>`` once a later frame has started or the render has ended, and a
frame with no new image fails the unit, to be retried.

When the artist's project folder is somewhere else on their machine (the
farm's path map has rules), the project is a text file naming their paths,
so a copy with the farm's paths is rendered instead, beside it, and removed
after. A ``.py`` in place of a project is run by Katana's script mode to
build one first (``katana --script=BUILD.py OUT.katana``), as the tool's
test does: a project file is not written by hand.

Katana is ``$KATANA_ROOT/katana``, else ``katana`` on PATH. The unit's
frames come from the farm's variables (``C4_FRAME_START``,
``C4_FRAME_END``, ``C4_FRAME_STEP``, or ``C4_FRAMES`` for a list); a list
is rendered as one batch per run of evenly spaced frames. Everything but
``main`` is plain Python, tested without Katana.
"""

import argparse
import gzip
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Callable, Dict, Iterable, List, Mapping, Optional, Tuple

#: Image files a render writes.
IMAGE_EXTENSIONS = (".exr", ".tif", ".tiff", ".png", ".jpg", ".jpeg", ".dpx", ".tga", ".hdr", ".deepexr")
#: A path to an image in a log line: absolute, ending in an image extension.
_IMAGE_IN_LINE = re.compile(r"((?:[A-Za-z]:)?[\\/][^\s'\"`<>|]*?\.(?:" +
                            "|".join(e[1:] for e in IMAGE_EXTENSIONS) + r"))(?=[\s'\"`]|$)", re.I)
_LAST_NUMBER = re.compile(r"(\d+)(?=\D*$)")


# --- the unit's frames ------------------------------------------------------------

def parse_frames(label: str) -> List[int]:
    """A frame list as the farm writes it: ``1001``, ``1001-1004``,
    ``1001-1010:2`` (or ``x2``), and lists of those joined by commas."""
    out: List[int] = []
    for part in (label or "").replace(" ", "").split(","):
        if not part:
            continue
        m = re.fullmatch(r"(-?\d+)(?:-(-?\d+)(?:[:x](\d+))?)?", part)
        if not m:
            raise ValueError(f"the frames {label!r} do not read ({part!r})")
        first = int(m.group(1))
        last = int(m.group(2)) if m.group(2) is not None else first
        step = int(m.group(3) or 1)
        if last < first or step < 1:
            raise ValueError(f"the frames {label!r} do not read ({part!r})")
        out += [f for f in range(first, last + 1, step) if f not in out]
    return out


def unit_frames(env: Mapping[str, str]) -> List[int]:
    """This unit's frames: ``C4_FRAMES`` when it is a list, else
    ``C4_FRAME_START`` to ``C4_FRAME_END`` by ``C4_FRAME_STEP``."""
    label = env.get("C4_FRAMES", "")
    if "," in label:
        return parse_frames(label)
    start = env.get("C4_FRAME_START", "")
    if not start:
        return parse_frames(label)
    end = env.get("C4_FRAME_END") or start
    step = env.get("C4_FRAME_STEP") or "1"
    return parse_frames(f"{start}-{end}:{step}")


def runs(frames: List[int]) -> List[Tuple[int, int, int]]:
    """Frames as (start, end, step) runs, each one batch render."""
    out: List[Tuple[int, int, int]] = []
    fs = sorted(set(frames))
    i = 0
    while i < len(fs):
        if i + 1 == len(fs):
            out.append((fs[i], fs[i], 1))
            break
        step = fs[i + 1] - fs[i]
        j = i + 1
        while j + 1 < len(fs) and fs[j + 1] - fs[j] == step:
            j += 1
        out.append((fs[i], fs[j], step))
        i = j + 1
    return out


def katana_frames(run: Tuple[int, int, int]) -> str:
    """A run as Katana's ``-t`` takes it: ``1001-1004``, ``1001``, or each
    frame of a stepped run joined by commas."""
    start, end, step = run
    if start == end:
        return str(start)
    if step == 1:
        return f"{start}-{end}"
    return ",".join(str(f) for f in range(start, end + 1, step))


# --- the farm's path map (OpenJD pathmapping-1.0) -----------------------------------------

def read_path_map(path: str) -> List[Tuple[str, str, str]]:
    """(format, source, destination) for each rule in the file
    ``C4_PATH_MAP`` names, longest source first; [] with no file."""
    if not path:
        return []
    with open(path, encoding="utf-8") as f:
        doc = json.load(f)
    rules = [(r.get("source_path_format", "POSIX").upper(), r["source_path"], r["destination_path"])
             for r in doc.get("path_mapping_rules") or []]
    return sorted(rules, key=lambda r: -len(r[1]))


def map_text(text: str, rules: List[Tuple[str, str, str]]) -> str:
    """A project's text with each source root the farm's: a root followed
    by a slash, a quote or the end of a value. A Windows root matches
    either slash and any case, and the rest of its path gets forward
    slashes."""
    for fmt, src, dst in rules:
        dst = dst.rstrip("/")
        if fmt == "WINDOWS":
            s = src.replace("\\", "/").rstrip("/")
            pat = re.compile("".join("[\\\\/]" if c == "/" else re.escape(c) for c in s) +
                             r"((?:[\\/][^\"'<>\s]*)?)(?=[\"'<>\s]|$)", re.I)
            text = pat.sub(lambda m: dst + m.group(1).replace("\\", "/"), text)
            continue
        s = src.rstrip("/")
        text = re.sub(re.escape(s) + r"(?=[/\"'<>\s]|$)", lambda m: dst, text)
    return text


def mapped_copy(project: str, rules: List[Tuple[str, str, str]]) -> Optional[str]:
    """A copy of the project with the farm's paths, beside it (so its
    relative paths still resolve); None when nothing in it changes. A
    gzipped project stays gzipped."""
    with open(project, "rb") as f:
        raw = f.read()
    zipped = raw[:2] == b"\x1f\x8b"
    text = (gzip.decompress(raw) if zipped else raw).decode("utf-8", errors="surrogateescape")
    out = map_text(text, rules)
    if out == text:
        return None
    data = out.encode("utf-8", errors="surrogateescape")
    d, name = os.path.split(project)
    copy = os.path.join(d, ".c4-mapped-" + name)
    with open(copy, "wb") as f:
        f.write(gzip.compress(data) if zipped else data)
    return copy


# --- Katana's side ----------------------------------------------------------------------------

def katana_exe(env: Mapping[str, str], which: Optional[Callable[[str], Optional[str]]] = None) -> Optional[str]:
    """``$KATANA_ROOT/katana`` (``katana.bat`` on Windows), else ``katana`` on PATH."""
    root = env.get("KATANA_ROOT", "")
    for name in ("katana", "katana.bat", "katana.exe"):
        p = os.path.join(root, name) if root else ""
        if p and os.path.isfile(p):
            return p
    return (which or shutil.which)("katana")


def batch_command(katana: str, project: str, node: str, run: Tuple[int, int, int], threads: int = 0) -> List[str]:
    """Katana's batch render of one Render node over one run of frames.
    ``threads`` above 0 sets the renderer's threads (``--threads3d``)."""
    cmd = [katana, "--batch", f"--katana-file={project}", f"--render-node={node}", "-t", katana_frames(run)]
    if threads > 0:
        cmd.append(f"--threads3d={threads}")
    return cmd


def build_command(katana: str, script: str, out: str) -> List[str]:
    """Katana's script mode running a ``.py`` that saves a project to ``out``."""
    return [katana, f"--script={script}", out]


# --- which frames are finished -----------------------------------------------------------------

def image_paths(line: str) -> List[str]:
    """The image files a log line names."""
    return [m.group(1) for m in _IMAGE_IN_LINE.finditer(line)]


def frame_of(path: str) -> Optional[int]:
    """A frame's number from its file's name: the last number before the extension."""
    stem = os.path.basename(path)
    for ext in IMAGE_EXTENSIONS:
        if stem.lower().endswith(ext):
            stem = stem[:-len(ext)]
            break
    m = _LAST_NUMBER.search(stem)
    return int(m.group(1)) if m else None


def new_images(top: str, since: float, frames: Iterable[int]) -> Dict[int, str]:
    """Image files under ``top`` written since ``since``, by frame, for
    the frames given: the first by name when a frame has several (its AOVs)."""
    want, out = set(frames), {}
    if not top or not os.path.isdir(top):
        return out
    for d, dirs, names in os.walk(top):
        dirs[:] = sorted(x for x in dirs if x != ".c4")
        for name in sorted(names):
            if not name.lower().endswith(IMAGE_EXTENSIONS):
                continue
            f = frame_of(name)
            if f not in want or f in out:
                continue
            p = os.path.join(d, name)
            try:
                if os.path.getmtime(p) >= since:
                    out[f] = p
            except OSError:
                continue
    return out


class Finished:
    """Reports frames as they finish. A batch render goes through its
    frames in order, so a file for one frame means every earlier frame
    with a file is done. Only files written during this run count, so a
    texture the log names is never taken for a frame."""

    def __init__(self, frames: List[int], since: float, say: Callable[[str], None]):
        self.frames, self.since, self.say = list(frames), since, say
        self.files: Dict[int, str] = {}
        self.done: List[int] = []

    def _new(self, path: str) -> bool:
        try:
            return os.path.getmtime(path) >= self.since
        except OSError:
            return True   # not there yet: being written; checked again when reported

    def saw(self, path: str) -> None:
        f = frame_of(path)
        if f not in self.frames or not self._new(path):
            return
        self.files.setdefault(f, path)
        for g in self.frames:
            if g < f and g in self.files:
                self.report(g)

    def report(self, frame: int) -> None:
        if frame in self.done or not os.path.isfile(self.files.get(frame, "")):
            return
        self.done.append(frame)
        self.say(f"c4_frame: {frame} {self.files[frame]}")
        self.say(f"openjd_progress: {round(100 * len(self.done) / len(self.frames))}")

    def finish(self, found: Mapping[int, str]) -> List[int]:
        """Report the rest, from the files found after the run; the frames
        still without a file."""
        for f in self.frames:
            if f not in self.files or not os.path.isfile(self.files[f]):
                if f in found:
                    self.files[f] = found[f]
            self.report(f)
        return [f for f in self.frames if f not in self.done]


# --- the render ---------------------------------------------------------------------------------

def fail(say: Callable[[str], None], kind: str, reason: str) -> int:
    say(f"c4_fail: {kind} {reason}")
    return 2 if kind == "permanent" else 1


def parse_args(argv: List[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="katana_render.py")
    p.add_argument("project")
    p.add_argument("--node", default="")
    p.add_argument("--threads", type=int, default=0)
    return p.parse_args(argv)


def _stream(popen, cmd, env, cwd, say, on_line=None) -> int:
    proc = popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env, cwd=cwd,
                 universal_newlines=True, errors="replace")
    for line in proc.stdout:
        line = line.rstrip("\n")
        say(line)
        if on_line:
            on_line(line)
    return proc.wait()


def main(argv: List[str], env: Mapping[str, str], say: Optional[Callable[[str], None]] = None,
         popen=subprocess.Popen, clock: Callable[[], float] = time.time) -> int:
    say = say or (lambda line: print(line, flush=True))
    try:
        a = parse_args(argv)
    except SystemExit:
        return fail(say, "permanent", "usage: katana_render.py PROJECT --node=NODE [--threads=N]")
    if not a.node:
        return fail(say, "permanent", "no Render node given (--node)")
    if not os.path.isfile(a.project):
        return fail(say, "permanent", f"no Katana project at {a.project}")
    if not a.project.lower().endswith((".katana", ".py")):
        return fail(say, "permanent", f"{os.path.basename(a.project)} is not a Katana project (.katana)")
    try:
        frames = unit_frames(env)
    except ValueError as e:
        return fail(say, "permanent", str(e))
    if not frames:
        return fail(say, "permanent", "the unit has no frames (C4_FRAME_START or C4_FRAMES)")
    try:
        rules = read_path_map(env.get("C4_PATH_MAP", ""))
    except (OSError, ValueError, KeyError) as e:
        return fail(say, "permanent", f"the farm's path map {env.get('C4_PATH_MAP')} does not read: {e}")
    katana = katana_exe(env)
    if not katana:
        return fail(say, "permanent", "no katana in $KATANA_ROOT or on PATH; run this tool on an image with Katana")

    cwd = os.path.dirname(os.path.abspath(a.project))
    child_env = dict(env)
    top = env.get("C4_TREE") or cwd
    project, scratch, made = a.project, None, None
    try:
        if project.lower().endswith(".py"):
            scratch = tempfile.mkdtemp(prefix="c4-katana-")
            built = os.path.join(scratch, "project.katana")
            say(f"openjd_status: building the project with {os.path.basename(project)}")
            code = _stream(popen, build_command(katana, project, built), child_env, cwd, say)
            if code != 0 or not os.path.isfile(built):
                return fail(say, "permanent", f"{os.path.basename(project)} did not build a project (exit {code})")
            project = built
        elif rules:
            made = mapped_copy(project, rules)
            project = made or project

        say(f"openjd_status: rendering {a.node}, frames {frames[0]}-{frames[-1]}")
        since = clock() - 1
        done = Finished(frames, since, say)

        def on_line(line: str) -> None:
            for p in image_paths(line):
                done.saw(p)

        for run in runs(frames):
            code = _stream(popen, batch_command(katana, project, a.node, run, a.threads), child_env, cwd, say, on_line)
            if code != 0:
                return fail(say, "retry", f"katana --batch exited {code} on frames {katana_frames(run)}")
        missing = done.finish(new_images(top, since, frames))
        if missing:
            return fail(say, "retry", f"katana --batch exited 0 but wrote no image for frame {missing[0]} under {top}")
        return 0
    finally:
        if made:
            try:
                os.remove(made)
            except OSError:
                pass
        if scratch:
            shutil.rmtree(scratch, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:], os.environ))
