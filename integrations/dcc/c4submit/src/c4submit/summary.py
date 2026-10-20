"""What a panel shows before Submit, from a check:

    sm0220-light-v004   Karma render · 240 frames · Karma XPU
      files      1,284 found from sm0220.usda · 6.2 GB · 214 to upload
      licences   sesi/karma, 1 per frame · 6 of 12 seats free now
      estimate   ready Thu 03:40 PDT (10:40 UTC), p90 05:10
      due        Thu 10:00 PDT (17:00 UTC) · client review · slack 4h 50m

Times are the artist's local time with UTC beside them. Every line has a
level (``ok``, ``warn``, ``error``) so a panel can colour it.
"""

import os
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import List, Optional

from c4submit import frames as framelib
from c4submit.submitter import Checked


@dataclass
class Line:
    label: str      # '' for a job's heading line
    text: str
    level: str = "ok"
    job: str = ""   # the job's key, when the line is about one job


def parse_time(s: str) -> Optional[datetime]:
    if not s:
        return None
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None


def clock(t: datetime, now: Optional[datetime] = None, utc: bool = True) -> str:
    """``Thu 03:40 PDT (10:40 UTC)``; the day is left out today."""
    local = t.astimezone()
    now = (now or datetime.now(timezone.utc)).astimezone()
    day = "" if local.date() == now.date() else local.strftime("%a ")
    out = f"{day}{local:%H:%M} {local.strftime('%Z') or ''}".rstrip()
    if utc and local.utcoffset():
        out += f" ({t.astimezone(timezone.utc):%H:%M} UTC)"
    return out


def duration(seconds: float) -> str:
    s = int(abs(seconds))
    d, rem = divmod(s, 86400)
    h, rem = divmod(rem, 3600)
    m = rem // 60
    if d:
        return f"{d}d {h}h" if h else f"{d}d"
    if h:
        return f"{h}h {m:02d}m" if m else f"{h}h"
    return f"{m}m"


def size(n: int) -> str:
    for unit, k in (("TB", 1e12), ("GB", 1e9), ("MB", 1e6), ("KB", 1e3)):
        if n >= k:
            return f"{n / k:.1f} {unit}"
    return f"{n} bytes"


def _estimate_lines(est: dict, key: str, now: Optional[datetime]) -> List[Line]:
    out = []
    eta50, eta90 = parse_time(est.get("eta50", "")), parse_time(est.get("eta90", ""))
    if eta50:
        text = f"ready {clock(eta50, now)}"
        if eta90:
            text += f", p90 {clock(eta90, now, utc=False)}"
        out.append(Line("estimate", text, job=key))
    due = parse_time(est.get("due") or est.get("deadline") or "")
    if due:
        src = est.get("dueSource") or est.get("deadlineSource") or ""
        if src.startswith("milestone "):
            src = src[len("milestone "):].replace("-", " ")
        elif src in ("suggested",):
            src = "suggested by the farm"
        else:
            src = ""
        slack = est.get("slackSeconds")
        text = clock(due, now) + (f" · {src}" if src else "")
        level = "ok"
        if isinstance(slack, (int, float)):
            if slack >= 0:
                text += f" · slack {duration(slack)}"
            else:
                text += f" · late by {duration(slack)}"
                level = "error"
            if est.get("atRisk") and slack >= 0:
                level = "warn"
        out.append(Line("due", text, level, job=key))
    for w in est.get("waits") or []:
        who = w.get("for", "")
        exp = w.get("expectSeconds")
        out.append(Line("waits", f"for {who}" + (f", usually {duration(exp)}" if exp else ""), "ok", job=key))
    return out


def lines(checked: Checked, now: Optional[datetime] = None, errors: bool = True) -> List[Line]:
    """The summary's lines; ``errors=False`` leaves out the errors and
    warnings, for a panel that shows them itself."""
    out: List[Line] = []
    for e in checked.errors if errors else []:
        where = e.get("path") or ""
        out.append(Line("error", (f"{where}: " if where else "") + e["message"] + (f" ({e['fix']})" if e.get("fix") else ""), "error"))
    for w in checked.warnings if errors else []:
        out.append(Line("warning", w, "warn"))
    d = checked.prepared.draft
    ans = checked.answer or {}
    farm_jobs = {j.get("key") or "": j for j in ans.get("jobs") or []}
    farm_list = list(ans.get("jobs") or [])
    col = checked.collection
    if col.files:
        scene = os.path.basename(d.scene_file) or "the scene"
        text = f"{len(col.files):,} found from {scene} · {size(col.bytes)}"
        if checked.farm_checked or checked.missing:
            text += f" · {len(checked.missing):,} to upload" + (f" ({size(checked.missing_bytes)})" if checked.missing else "")
        out.append(Line("files", text))
    for i, (key, job) in enumerate(checked.doc.get("jobs", {}).items()):
        fj = farm_jobs.get(key) or (farm_list[i] if i < len(farm_list) else {})
        name = fj.get("name") or f"{checked.doc.get('shot', '')}-{key}".strip("-")
        head = [d.tool.title]
        if job.get("frames"):
            try:
                n = len(framelib.parse(job["frames"]))
                head.append(f"{n:,} frame{'s' if n != 1 else ''}")
            except ValueError:
                head.append(str(job["frames"]))
        if fj.get("units"):
            head.append(f"{fj['units']:,} units" + (f" ({fj['expected']})" if fj.get("expected") else ""))
        if job.get("after"):
            after = job["after"] if isinstance(job["after"], list) else [job["after"]]
            head.append("after " + ", ".join(after))
        out.append(Line("", f"{name}   " + " · ".join(head), job=key))
        for lic in fj.get("licences") or []:
            seats = f"{lic.get('free')} of {lic.get('total')} seats free now" if lic.get("total") else ""
            per = lic.get("perUnit", 1)
            text = f"{lic.get('pool')}/{lic.get('feature')}, {per} per unit" + (f" · {seats}" if seats else "")
            out.append(Line("licences", text, "warn" if lic.get("free", 1) == 0 else "ok", job=key))
        est = fj.get("estimate") or (ans.get("estimate") if len(checked.doc.get("jobs", {})) == 1 else None)
        if est:
            out += _estimate_lines(est, key, now)
    if ans.get("estimate") and len(checked.doc.get("jobs", {})) > 1 and not all(j.get("estimate") for j in farm_list):
        out += _estimate_lines(ans["estimate"], "", now)
    return out


def text(checked: Checked, now: Optional[datetime] = None, errors: bool = True) -> str:
    """The summary as plain text, for a panel without rich text."""
    rows = []
    for ln in lines(checked, now, errors):
        rows.append(ln.text if not ln.label else f"  {ln.label:<10} {ln.text}")
    return "\n".join(rows)
