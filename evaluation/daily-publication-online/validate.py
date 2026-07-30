#!/usr/bin/env python3
"""Validate one runner artifact with the authoritative offline harness."""

from __future__ import annotations

import argparse
import importlib.util
import pathlib
import sys


def load_harness(path: pathlib.Path):
    spec = importlib.util.spec_from_file_location("daily_publication_harness", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot import {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--harness", type=pathlib.Path, required=True)
    parser.add_argument("--evidence", type=pathlib.Path, required=True)
    arguments = parser.parse_args()
    harness = load_harness(arguments.harness)
    try:
        online, gates = harness.online_evidence(arguments.evidence)
    except harness.EvidenceError as exc:
        print(f"daily-publication-online: {exc}", file=sys.stderr)
        return 1
    if online.get("status") != "complete" or any(gate.get("status") != "pass" for gate in gates):
        print("daily-publication-online: authoritative online gates did not all pass", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
