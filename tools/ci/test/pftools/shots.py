"""Shot names and frame ranges, as Petrel & Finch writes them."""
import re

SHOT = re.compile(r"^(?P<show>[a-z]{2})(?P<num>\d{4})$")


def parse(name):
    m = SHOT.match(name)
    if not m:
        raise ValueError(f"{name!r} is not a shot name like sm0140")
    return m["show"], int(m["num"])


def frames(spec):
    first, _, last = spec.partition("-")
    return list(range(int(first), int(last or first) + 1))


def review_name(shot, version):
    return f"{shot}_review_v{version:03d}.mov"
