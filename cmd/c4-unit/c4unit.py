"""The C4 unit contract for Python job code: the same calls as c4-unit.

    import c4unit
    c4unit.progress(0.4)
    c4unit.preview("/show/render/sm0140.1024.exr", 1024)
    if c4unit.yield_requested():
        save_checkpoint(c4unit.output()); raise SystemExit(0)
    # at each safe point (a frame done, a checkpoint saved, a turn ended):
    if c4unit.safepoint(checkpoint="step-4000"):
        raise SystemExit(0)          # the farm asked the unit to stop here
    start = c4unit.resume_dir()      # None on a first attempt

Every C4 job image installs this module and the c4-unit binary it calls.
"""

import subprocess

_BIN = "c4-unit"


def _run(*args, check=True):
    return subprocess.run([_BIN, *map(str, args)], check=check, capture_output=True, text=True)


def progress(fraction):
    """Report the unit's progress, a number from 0 to 1."""
    _run("progress", fraction)


def preview(image, frame, width=960):
    """Write the HUD's preview of a frame (EXR, TIFF, PNG or JPEG)."""
    _run("preview", "--width", width, image, frame)


def yield_requested():
    """True when the farm has asked this unit to stop at its next checkpoint."""
    return _run("yield-requested", check=False).returncode == 0


def safepoint(checkpoint=None, wait=None):
    """At a safe point, a moment the unit could stop and lose nothing it
    wants kept, ask the farm whether to stop there. True when the answer is
    stop: exit there, and the next attempt resumes from what it wrote.
    wait (seconds, default 20) is how long to wait for the answer before
    falling back on the farm's last request."""
    args = ["safepoint"] + (["--checkpoint", checkpoint] if checkpoint else [])
    if wait is not None:
        args += ["--wait", f"{wait}s"]
    r = _run(*args, check=False)
    if r.returncode not in (0, 10):
        raise subprocess.CalledProcessError(r.returncode, [_BIN, *args], r.stdout, r.stderr)
    return r.returncode == 10


def measure(value, of=None, name=None, detail=None):
    """Report the job's measure of done: value of the target (of, else the
    job's), such as 361 of 379 rules sited."""
    args = ["measure"]
    if of is not None:
        args += ["--of", str(of)]
    if name:
        args += ["--name", name]
    if detail:
        args += ["--detail", detail]
    _run(*args, str(value))


def usage(model, input_tokens=0, output_tokens=0, cache_read_tokens=0, cost_usd=None, cost_source=None):
    """Add a model call's tokens, and its cost if known, to the unit's spend."""
    args = ["usage", "--model", model, "--input", str(input_tokens), "--output", str(output_tokens),
            "--cache-read", str(cache_read_tokens)]
    if cost_usd is not None:
        args += ["--cost", repr(float(cost_usd))]
    if cost_source:
        args += ["--cost-source", cost_source]
    _run(*args)


def output():
    """The unit's writable mount."""
    return _run("output").stdout.strip()


def resume_dir():
    """The writable mount if this attempt resumes a yielded one, else None."""
    out = _run("resume-dir").stdout.strip()
    return out or None
