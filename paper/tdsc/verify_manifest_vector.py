#!/usr/bin/env python3
"""Independently verify the paper's AuthorizationManifestV1 digest vector."""

from __future__ import annotations

import hashlib
import json
import pathlib


ROOT = pathlib.Path(__file__).resolve().parents[2]
VECTOR = ROOT / "testdata" / "authorization-manifest-v1-vector.json"
DOMAIN = b"TASKGATE-MANIFEST-V1\0"


def main() -> None:
    vector = json.loads(VECTOR.read_text(encoding="utf-8"))
    # This fixed vector contains ASCII property names and integral numbers, so
    # Python's sorted compact encoding is byte-identical to RFC 8785 JCS. The
    # production Go implementation separately tests UTF-16 ordering and
    # ECMAScript number serialization.
    canonical = json.dumps(
        vector["manifest"], ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")
    actual = hashlib.sha256(DOMAIN + canonical).hexdigest()
    if actual != vector["digest"]:
        raise SystemExit(
            f"manifest vector mismatch: calculated {actual}, expected {vector['digest']}"
        )
    print(f"manifest vector verified: {actual}")


if __name__ == "__main__":
    main()
