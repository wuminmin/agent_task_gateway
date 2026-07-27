#!/usr/bin/env python3
"""Validate and bind a PostgreSQL multi-scale report to its implementation."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SOURCE_DIRS = (
    "evaluation/cmd/exposure-bench",
    "internal/control",
    "internal/exposure",
    "internal/gateway",
    "internal/queryplan",
)
SOURCE_FILES = (
    "go.mod",
    "go.sum",
    "cmd/gateway/main.go",
    "evaluation/exposure-scale/05-scale-data.sql",
    "evaluation/exposure-scale/15-scale-reader.sql",
    "evaluation/exposure-scale/catalog.yaml",
    "evaluation/exposure-scale/compose.yaml",
    "evaluation/exposure-scale/finalize.py",
    "evaluation/run-exposure-scale.sh",
)


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def source_sha256() -> str:
    paths = [ROOT / relative for relative in SOURCE_FILES]
    for relative in SOURCE_DIRS:
        paths.extend(
            path
            for path in (ROOT / relative).rglob("*")
            if path.suffix in {".go", ".sql"}
        )
    digest = hashlib.sha256()
    for path in sorted(set(paths)):
        digest.update(path.relative_to(ROOT).as_posix().encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def validate(report: dict, allow_smoke: bool = False) -> None:
    require(report.get("schema_version") == 1, "unsupported scale schema")
    require(
        report.get("status") == "complete_postgresql16_multiscale_join_group_campaign",
        "scale campaign is incomplete",
    )
    require(
        re.match(r"^16\.", report.get("postgres_version", "")) is not None,
        "scale campaign did not use PostgreSQL 16",
    )
    config = report.get("configuration", {})
    sizes = config.get("orders_per_scale", [])
    trials = config.get("trials")
    require(sizes == sorted(set(sizes)) and trials >= 1, "invalid scale dimensions")
    if not allow_smoke:
        require(
            len(sizes) >= 3 and sizes[-1] * 23 >= 1_000_000 and trials >= 3,
            "scale cardinalities do not include three scales and one million facts",
        )
    points = {
        (point.get("orders"), point.get("trial"), point.get("operation")): point
        for point in report.get("raw_points", [])
    }
    require(len(points) == len(sizes) * trials * 3, "scale raw points are incomplete")
    for size in sizes:
        for trial in range(1, trials + 1):
            direct = points.get((size, trial, "direct_sql"), {})
            novel = points.get((size, trial, "novel"), {})
            replay = points.get((size, trial, "replay"), {})
            expected = size * 23
            require(
                direct.get("rows") == novel.get("rows") == replay.get("rows") == 3
                and novel.get("expected_influence_facts") == expected
                and novel.get("actual_influence_facts") == expected
                and novel.get("charged_influence_facts") == expected
                and novel.get("actual_release_facts", 0) > 0
                and novel.get("charged_release_facts") == novel.get("actual_release_facts")
                and replay.get("actual_influence_facts") == expected
                and replay.get("actual_release_facts") == novel.get("actual_release_facts")
                and replay.get("charged_influence_facts", 0) == 0
                and replay.get("charged_release_facts", 0) == 0
                and replay.get("observation_sha256") == novel.get("observation_sha256")
                and replay.get("ledger_before", {}).get("fact_rows")
                == replay.get("ledger_after", {}).get("fact_rows")
                == novel.get("ledger_after", {}).get("fact_rows"),
                f"invalid accounting at scale={size}, trial={trial}",
            )
    aggregates = {
        (item.get("orders"), item.get("operation")): item
        for item in report.get("aggregates", [])
    }
    require(
        len(aggregates) == len(sizes) * 3
        and all(item.get("trials") == trials for item in aggregates.values()),
        "scale aggregates are incomplete",
    )
    peaks = report.get("service_peak_memory_bytes", {})
    require(
        set(peaks) == {"control-postgres", "business-postgres", "gateway"}
        and all(value > 0 for value in peaks.values()),
        "service peak-memory evidence is incomplete",
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--raw-relative", required=True)
    parser.add_argument("--allow-smoke", action="store_true")
    args = parser.parse_args()
    report = json.loads(args.report.read_text(encoding="utf-8"))
    validate(report, args.allow_smoke)
    report["source_sha256"] = source_sha256()
    report["raw_provenance"] = {
        "artifact": args.raw_relative,
        "artifact_sha256": sha256(args.report),
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )


if __name__ == "__main__":
    main()
