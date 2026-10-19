"""C4 IDs (SMPTE ST 2114): the SHA-512 of the bytes, base58-encoded,
padded to 88 characters and prefixed "c4". The same computation as the Go
library's c4.Identify and ID.String."""

import hashlib
from typing import BinaryIO, Union

_CHARSET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
_LEN = 90


def encode(digest: bytes) -> str:
    """The C4 ID string of a 64-byte SHA-512 digest."""
    if len(digest) != 64:
        raise ValueError("a C4 ID encodes a 64-byte SHA-512 digest")
    n = int.from_bytes(digest, "big")
    out = ["1"] * _LEN
    out[0], out[1] = "c", "4"
    i = _LEN - 1
    while i > 1 and n > 0:
        n, r = divmod(n, 58)
        out[i] = _CHARSET[r]
        i -= 1
    return "".join(out)


def identify(data: Union[bytes, BinaryIO]) -> str:
    """The C4 ID of bytes, or of everything a binary stream yields."""
    h = hashlib.sha512()
    if isinstance(data, (bytes, bytearray, memoryview)):
        h.update(data)
    else:
        for chunk in iter(lambda: data.read(1 << 20), b""):
            h.update(chunk)
    return encode(h.digest())


def is_id(s: str) -> bool:
    """Whether s has the shape of a C4 ID."""
    return len(s) == _LEN and s.startswith("c4") and all(c in _CHARSET for c in s[2:])
