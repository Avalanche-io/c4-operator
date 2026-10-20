"""The submission (``c4: submit/1``, SUBMIT.md): what a panel writes and
sends. This module checks a document locally the way the farm does, so a
panel can point at a mistake before anything is sent. The farm's check
(``POST /submissions`` with ``dryRun``) is the authority; this one only
answers sooner, with errors of the same shape: ``{path, message, fix}``.
"""

import datetime
import difflib
import re
from typing import Any, Dict, List

from c4submit import frames as framelib
from c4submit.c4id import is_id

FORMAT = "submit/1"

TOP = ("c4", "project", "shot", "name", "due", "tree", "root", "read", "tracker", "for", "note", "jobs")
JOB = ("tool", "frames", "with", "after", "due", "size", "needs", "pools", "preview", "check", "approve",
       "wait", "answers", "uses", "plan", "each", "over", "combine", "measure", "retries", "maxParallel",
       "chunk", "order", "follows")
AFTER_HOW = ("frames", "all", "previous")

KEY = re.compile(r"^[a-z][a-z0-9-]{0,39}$")
_TOOL = re.compile(r"^[a-z][a-z0-9-]*(@(v\d+|c4[1-9A-HJ-NP-Za-km-z]{88}))?$")
_MILESTONE = re.compile(r"^[a-z][a-z0-9-]*$")
_DAY = r"(mon|tue|wed|thu|fri|sat|sun)"
_DUE_TIME = re.compile(r"^(%s\s+)?([01]?\d|2[0-3]):[0-5]\d$" % _DAY, re.I)
_DUE_REL = re.compile(r"^\+\d+(\.\d+)?[mhd]$")
_DATE = re.compile(r"^\d{4}-\d\d-\d\d$")
_RFC3339 = re.compile(r"^\d{4}-\d\d-\d\dT\d\d:\d\d(:\d\d(\.\d+)?)?(Z|[+-]\d\d:\d\d)$")


def error(path: str, message: str, fix: str = "") -> Dict[str, str]:
    e = {"path": path, "message": message}
    if fix:
        e["fix"] = fix
    return e


def _did_you_mean(word: str, choices) -> str:
    m = difflib.get_close_matches(word, list(choices), n=1, cutoff=0.6)
    return f'did you mean "{m[0]}"?' if m else ""


def due_problem(due: Any) -> str:
    """What is wrong with a ``due`` value, or ''. A YAML 1.1 reader turns an
    unquoted date or time into a date or datetime, which is the same due."""
    if isinstance(due, (datetime.date, datetime.datetime)):
        return ""
    if not isinstance(due, str) or not due.strip():
        return "due is a milestone, a time (\"fri 18:00\", \"+3h\"), a date or \"suggested\""
    s = due.strip()
    if (s == "suggested" or _DUE_TIME.match(s) or _DUE_REL.match(s) or _DATE.match(s) or _RFC3339.match(s)
            or _MILESTONE.match(s)):
        return ""
    return f'"{s}" is not a milestone, a time ("fri 18:00", "+3h"), a date or "suggested"'


def tree_problem(tree: Any) -> str:
    if not isinstance(tree, str) or not tree:
        return "tree is a folder, a C4 ID, job:<name> or git:<repo>@<ref>"
    if tree.startswith("c4") and len(tree) == 90 and not is_id(tree):
        return f'"{tree}" is not a C4 ID'
    if tree.startswith("job:") and not re.match(r"^job:[a-z0-9][a-z0-9.-]*$", tree):
        return f'"{tree}" does not name a job'
    if tree.startswith("git:") and "@" not in tree:
        return "a git tree names its ref: git:<repo>@<ref>"
    return ""


def _after_keys(after: Any) -> List[str]:
    if isinstance(after, str):
        return [after]
    if isinstance(after, list):
        return [a for a in after if isinstance(a, str)]
    if isinstance(after, dict):
        return list(after)
    return []


def check(doc: Any) -> List[Dict[str, str]]:
    """Structural problems with a submission, in document order."""
    errs: List[Dict[str, str]] = []
    if not isinstance(doc, dict):
        return [error("", "a submission is a map of fields", "start it with c4: submit/1")]
    if doc.get("c4") != FORMAT:
        errs.append(error("c4", f'c4 must be "{FORMAT}"' if "c4" in doc else "c4 is missing", f"add c4: {FORMAT} as the first line"))
    for k in doc:
        if k not in TOP:
            hint = _did_you_mean(k, TOP)
            errs.append(error(k, f'submit/1 has no field "{k}"', hint or "remove it; SUBMIT.md section 3 lists the fields"))
    if "due" in doc:
        p = due_problem(doc["due"])
        if p:
            errs.append(error("due", p, "pick a milestone, or write a time like \"fri 18:00\""))
    if "tree" in doc:
        p = tree_problem(doc["tree"])
        if p:
            errs.append(error("tree", p))
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict) or not jobs:
        errs.append(error("jobs", "a submission has at least one job", "add jobs: with a key and a tool"))
        return errs
    for key, job in jobs.items():
        base = f"jobs.{key}"
        if not KEY.match(str(key)):
            errs.append(error(base, f'"{key}" is not a job key', "lowercase letters, digits and -, starting with a letter"))
        if not isinstance(job, dict):
            errs.append(error(base, "a job is a map of fields", "give it at least tool:"))
            continue
        for k in job:
            if k not in JOB:
                hint = _did_you_mean(k, JOB)
                errs.append(error(f"{base}.{k}", f'a job has no field "{k}"', hint or "remove it; SUBMIT.md section 4 lists a job's fields"))
        if "tool" not in job and "plan" not in job:
            errs.append(error(base, "a job names its tool", "add tool: and the tool's name"))
        elif "tool" in job and not (isinstance(job["tool"], str) and _TOOL.match(job["tool"])):
            errs.append(error(f"{base}.tool", f'"{job["tool"]}" is not a tool name', "a tool's name, or name@v4, or name@<C4 ID>"))
        if "frames" in job:
            p = framelib.check(job["frames"])
            if p:
                errs.append(error(f"{base}.frames", p))
        if "due" in job:
            p = due_problem(job["due"])
            if p:
                errs.append(error(f"{base}.due", p))
        if "with" in job and not isinstance(job["with"], dict):
            errs.append(error(f"{base}.with", "with is a map of the tool's parameters"))
        after = job.get("after")
        if after is not None:
            if isinstance(after, dict):
                for k, how in after.items():
                    if how not in AFTER_HOW:
                        errs.append(error(f"{base}.after.{k}", f'"{how}" is not a way to follow a job', "frames, all or previous"))
            for k in _after_keys(after):
                if k == key:
                    errs.append(error(f"{base}.after", "a job cannot follow itself"))
                elif k not in jobs:
                    hint = _did_you_mean(k, [j for j in jobs if j != key])
                    errs.append(error(f"{base}.after", f'no job "{k}" in this submission',
                                      "jobs follow jobs by their key in the file" + (f"; {hint}" if hint else "")))
    cycle = _cycle(jobs)
    if cycle:
        errs.append(error(f"jobs.{cycle[0]}.after", "these jobs follow each other in a circle: " + " → ".join(cycle + cycle[:1]),
                          "one of them has to go first"))
    return errs


def _cycle(jobs: dict) -> List[str]:
    edges = {k: [a for a in _after_keys(j.get("after")) if a in jobs and a != k] if isinstance(j, dict) else []
             for k, j in jobs.items()}
    state: Dict[str, int] = {}
    stack: List[str] = []

    def visit(k):
        state[k] = 1
        stack.append(k)
        for a in edges[k]:
            if state.get(a) == 1:
                return stack[stack.index(a):]
            if a not in state:
                c = visit(a)
                if c:
                    return c
        stack.pop()
        state[k] = 2
        return None

    for k in jobs:
        if k not in state:
            c = visit(k)
            if c:
                return list(c)
    return []


def job_key(stage: str, name: str = "", several: bool = False) -> str:
    """A job's key: the stage alone for one renderable, ``stage-name`` for
    one of several, from a node's name lower-cased with anything outside
    ``[a-z0-9-]`` made ``-`` (and a leading ``write`` dropped)."""
    stage = _slug(stage) or "render"
    if not several or not name:
        return stage[:40]
    n = _slug(name)
    n = re.sub(r"^write-?", "", n) or n
    return f"{stage}-{n}"[:40].rstrip("-") if n else stage


def _slug(s: str) -> str:
    s = re.sub(r"[^a-z0-9-]+", "-", (s or "").lower()).strip("-")
    s = re.sub(r"-+", "-", s)
    if s and not s[0].isalpha():
        s = "j-" + s
    return s
