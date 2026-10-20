"""The submission (``c4: submit/1``): what a panel writes and
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
from c4submit import paths
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
    if "c4" not in doc:
        errs.append(error("c4", "the file has no c4 field, so it is not a submission", f"add c4: {FORMAT} as the first line"))
    elif doc["c4"] != FORMAT:
        errs.append(error("c4", f"{doc['c4']} is not a format this farm reads", f"this farm reads {FORMAT}"))
    for k in doc:
        if k not in TOP:
            errs.append(_unknown(k, k, TOP, "a submission"))
    if "root" in doc and not (isinstance(doc["root"], str) and paths.is_absolute(doc["root"])):
        errs.append(error("root", f'"{doc["root"]}" is not an absolute path',
                          "write the folder the tree came from, as S:/shows/saltmarsh or /Users/noor/shows/saltmarsh"))
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
        stem = str(doc.get("name") or doc.get("shot") or "")
        if stem and len(f"{stem}-{key}-v000") > 63:
            errs.append(error(base, f"{stem}-{key}-v001 is longer than a job's name may be (63 characters)",
                              "shorten the job's key, or set name: to a shorter stem"))
        if not isinstance(job, dict):
            errs.append(error(base, "a job is a map of fields", "give it at least tool:"))
            continue
        for k in job:
            if k not in JOB:
                errs.append(_unknown(f"{base}.{k}", k, JOB, "a job"))
        if "tool" not in job and "plan" not in job:
            errs.append(error(base, "a job needs a tool, or a plan with each", "add tool: <name>; c4 farm tools lists them"))
        elif "tool" in job and not (isinstance(job["tool"], str) and _TOOL.match(job["tool"])):
            errs.append(error(f"{base}.tool", f'"{job["tool"]}" is not a tool name', "a tool's name, or name@v4, or name@<C4 ID>"))
        if "frames" in job and framelib.check(job["frames"]):
            fr = str(job["frames"])
            errs.append(error(f"{base}.frames", f'"{fr}" is not a frame list',
                              f"write a range with a dash: {fr.replace('..', '-')}" if ".." in fr
                              else "write frames as 1001-1240, 1001-1240:2 or 1001,1050-1100"))
        needs = job.get("needs")
        if isinstance(needs, dict) and "licences" in needs:
            errs.append(error(f"{base}.needs.licences", "licences are the tool's to declare, not a submission's",
                              "ask the tool's owner to add them to its needs, or choose one of its sizes"))
        answers = job.get("answers")
        by = answers.get("by") if isinstance(answers, dict) else None
        if isinstance(by, str) and by.split(":", 1)[0] in ("person", "party", "role"):
            errs.append(error(f"{base}.answers.by", "a person is never asked mid-run: questions go to an agent",
                              "name an agent (lead, tool:<name> or unit:<name>); for a person's decision, add an approve step"))
        if "combine" in job and "over" not in job:
            errs.append(error(f"{base}.combine", "combine needs over: the values it combines", "add over:, or remove combine"))
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
                    # A key written as a job's versioned name (comp-v4) gets the reason too.
                    why = "jobs follow jobs by their key in the file" if re.search(r"-v\d+$", k) or not hint else ""
                    errs.append(error(f"{base}.after", f'no job "{k}" in this submission', "; ".join(x for x in (why, hint) if x)))
    cycle = _cycle(jobs)
    if cycle:
        names = ", ".join(cycle[:-1]) + " and " + cycle[-1]
        errs.append(error(f"jobs.{cycle[-1]}.after", f"{names} follow each other, which is a cycle",
                          "remove one of the two after fields" if len(cycle) == 2 else "remove one of their after fields"))
    return errs


# Fields people bring from other queues, each refused with the reason.
_REFUSED = {
    "priority": ("{what} has no priority: work is ordered by when it is due",
                 "say when it is needed with due: a milestone, a time, a date or suggested"),
    "image": ("a submission names tools, never images",
              "publish a tool that runs the image (c4 farm tool new), and name it in tool:"),
}


def _unknown(path: str, k: str, fields, what: str) -> Dict[str, str]:
    if k in _REFUSED:
        msg, fix = _REFUSED[k]
        return error(path, msg.format(what=what), fix)
    hint = _did_you_mean(k, fields)
    return error(path, f'"{k}" is not a field of {what}', hint or "GET /schema/submit lists the fields")


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
