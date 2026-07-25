#!/usr/bin/env python3
"""Merge Docker stats peak memory into an exposure benchmark report."""

import argparse
import json
import re
from pathlib import Path


UNITS = {
    "B": 1,
    "KB": 1000,
    "MB": 1000**2,
    "GB": 1000**3,
    "KIB": 1024,
    "MIB": 1024**2,
    "GIB": 1024**3,
}
ANSI = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]")


def memory_bytes(value: str) -> int:
    used = value.split("/", 1)[0].strip().replace(" ", "")
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)([A-Za-z]+)", used)
    if not match:
        raise ValueError(f"unrecognized Docker memory value: {value!r}")
    return round(float(match.group(1)) * UNITS[match.group(2).upper()])


def service_name(container: str) -> str | None:
    for service in ("control-postgres", "business-postgres", "gateway"):
        if re.search(rf"(?:^|-){re.escape(service)}-\d+$", container):
            return service
    return None


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--stats", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    report = json.loads(args.report.read_text(encoding="utf-8"))
    peaks: dict[str, int] = {}
    observations: dict[str, int] = {}
    for line in args.stats.read_text(encoding="utf-8").splitlines():
        # Docker stats may inherit a PTY and wrap JSON rows in cursor-control
        # sequences. Strip only ANSI controls, then require one JSON object.
        line = ANSI.sub("", line).strip()
        start, end = line.find("{"), line.rfind("}")
        if start < 0 or end < start:
            continue
        row = json.loads(line[start : end + 1])
        service = service_name(row.get("Name", ""))
        if service is None:
            continue
        value = memory_bytes(row["MemUsage"])
        peaks[service] = max(peaks.get(service, 0), value)
        observations[service] = observations.get(service, 0) + 1
    if set(peaks) != {"control-postgres", "business-postgres", "gateway"}:
        raise SystemExit(f"missing Docker memory samples: {sorted(peaks)}")
    report["service_peak_memory_bytes"] = peaks
    report["configuration"]["docker_stats_observations"] = observations
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
