#!/usr/bin/env python3
"""Validate raw RQ4 samples and reproduce the published campaign summary."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import statistics
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
DEFAULT_RUNS = [f"rq4-20260728-trial{index}" for index in range(1, 4)]
SOURCE_PATHS = [
    ROOT / "internal",
    ROOT / "cmd/gateway",
    ROOT / "evaluation/cmd/exposure-bench",
    ROOT / "go.mod",
    ROOT / "go.sum",
    ROOT / "compose.yaml",
    ROOT / "evaluation/Dockerfile",
    ROOT / "evaluation/run-exposure-performance.sh",
    HERE / "compose.yaml",
    HERE / "catalog.yaml",
    HERE / "merge_memory.py",
    HERE / "summarize_campaign.py",
]


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def tree_digest(paths: list[Path]) -> str:
    checksum = hashlib.sha256()
    files: list[Path] = []
    for path in paths:
        files.extend(path.rglob("*.go") if path.is_dir() else [path])
    for path in sorted(set(files)):
        relative = path.relative_to(ROOT).as_posix()
        checksum.update(relative.encode())
        checksum.update(b"\0")
        checksum.update(path.read_bytes())
        checksum.update(b"\0")
    return checksum.hexdigest()


def median(rows: list[dict], path: tuple[str, ...]) -> float:
    values = []
    for row in rows:
        value = row
        for key in path:
            value = value[key]
        values.append(float(value))
    return statistics.median(values)


def percentile(values: list[float], probability: float) -> float:
    """Hyndman--Fan type 7, matching the benchmark implementation."""
    ordered = sorted(values)
    if not ordered:
        raise ValueError("percentile of empty sample")
    position = (len(ordered) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def close(actual: float, expected: float) -> bool:
    return math.isclose(actual, expected, rel_tol=1e-12, abs_tol=1e-9)


def validate_metric(values: list[float], reported: dict, label: str) -> None:
    expected = {
        "count": len(values),
        "min": min(values),
        "p50": percentile(values, 0.50),
        "p95": percentile(values, 0.95),
        "p99": percentile(values, 0.99),
        "max": max(values),
        "mean": statistics.fmean(values),
    }
    for key, value in expected.items():
        if key not in reported or not close(float(reported[key]), float(value)):
            raise ValueError(f"{label} {key} does not match raw samples")


def validate_samples(run_id: str, report: dict, samples_path: Path) -> int:
    cells = {(cell["phase"], int(cell["concurrency"])): cell for cell in report["cells"]}
    if len(cells) != len(report["cells"]) or len(cells) != 13:
        raise ValueError(f"cell manifest mismatch in {run_id}")
    samples: dict[tuple[str, int], list[dict]] = {key: [] for key in cells}
    with samples_path.open("r", encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            if not line.strip():
                continue
            try:
                row = json.loads(line)
            except json.JSONDecodeError as error:
                raise ValueError(f"invalid JSON in {run_id} samples line {line_number}") from error
            key = (row.get("phase"), row.get("concurrency"))
            if key not in samples:
                raise ValueError(f"unknown sample cell {key} in {run_id}")
            for field in (
                "worker", "iteration", "latency_ms", "rows", "actual_release_facts",
                "actual_influence_facts", "charged_release_facts", "charged_influence_facts",
            ):
                if field not in row or not isinstance(row[field], (int, float)) or row[field] < 0:
                    raise ValueError(f"invalid {field} in {run_id} samples line {line_number}")
            if row["charged_release_facts"] > row["actual_release_facts"] or \
                    row["charged_influence_facts"] > row["actual_influence_facts"]:
                raise ValueError(f"charge exceeds actual facts in {run_id} samples line {line_number}")
            if row["phase"].startswith("full_history_"):
                observation = row.get("observation_sha256", "")
                if len(observation) != 64 or any(character not in "0123456789abcdef" for character in observation):
                    raise ValueError(f"full-path sample lacks an observation digest in {run_id} line {line_number}")
            samples[key].append(row)

    for key, cell in cells.items():
        rows = samples[key]
        if len(rows) != cell["samples"]:
            raise ValueError(f"sample count mismatch for {run_id} {key}")
        validate_metric([float(row["latency_ms"]) for row in rows], cell["latency_ms"], f"{run_id} {key} latency")
        if "database_ms" in cell:
            validate_metric([float(row["database_ms"]) for row in rows], cell["database_ms"], f"{run_id} {key} database")
        for component, reported in cell.get("component_ms", {}).items():
            values = [float(row["component_ms"][component]) for row in rows]
            validate_metric(values, reported, f"{run_id} {key} component {component}")
        if key[0] == "full_history_hit":
            if any(row["actual_release_facts"] + row["actual_influence_facts"] == 0 or
                   row["charged_release_facts"] + row["charged_influence_facts"] != 0 for row in rows):
                raise ValueError(f"history-hit raw samples are not complete hits in {run_id} {key}")
            if cell.get("fact_history_hit_rate") != 1 or cell.get("query_history_hit_rate") != 1:
                raise ValueError(f"history-hit cell rates are not one in {run_id} {key}")
            if any(cell["ledger_growth"][name] != 0 for name in ("release_used", "influence_used", "fact_rows")):
                raise ValueError(f"history-hit cell grew the ledger in {run_id} {key}")
    return sum(len(rows) for rows in samples.values())


def build_summary(run_ids: list[str]) -> dict:
    raw_root = HERE / "raw"
    trials = []
    provenance = []
    expected_configuration = None
    expected = {
        "cache_strategy": "warm", "concurrency": [1, 4, 8], "ramp_runs": 32,
        "runs_per_worker": 200, "task_concurrency_mode": "delegated_tasks_shared_root",
        "workload": "expense_detail/sales/ordered/limit-1",
    }
    for run_id in run_ids:
        if not run_id.startswith("rq4-") or "/" in run_id or ".." in run_id:
            raise ValueError(f"unsafe RQ4 run id {run_id!r}")
        run_dir = raw_root / run_id
        result_path = run_dir / "results.json"
        report_path = run_dir / "report.json"
        samples_path = run_dir / "samples.jsonl"
        stats_path = run_dir / "docker-stats.jsonl"
        for path in (result_path, report_path, samples_path, stats_path):
            if not path.is_file():
                raise ValueError(f"missing published raw artifact {path.relative_to(ROOT)}")
        report = json.loads(result_path.read_text(encoding="utf-8"))
        driver_report = json.loads(report_path.read_text(encoding="utf-8"))
        if report.get("schema_version") != 1 or report.get("status") != "smoke":
            raise ValueError(f"invalid raw trial {run_id}")
        merged_without_memory = dict(report)
        merged_without_memory.pop("service_peak_memory_bytes", None)
        merged_configuration = dict(merged_without_memory["configuration"])
        merged_configuration.pop("docker_stats_observations", None)
        merged_without_memory["configuration"] = merged_configuration
        if merged_without_memory != driver_report:
            raise ValueError(f"merged result does not preserve driver report in {run_id}")
        configuration = report["configuration"]
        comparable = {key: configuration[key] for key in expected}
        if expected_configuration is None:
            expected_configuration = comparable
        elif comparable != expected_configuration:
            raise ValueError(f"configuration mismatch in {run_id}")
        if comparable != expected:
            raise ValueError(f"unexpected publication configuration in {run_id}: {comparable}")
        sample_count = validate_samples(run_id, report, samples_path)
        if sample_count != 10432 or sum(cell["samples"] for cell in report["cells"]) != sample_count:
            raise ValueError(f"sample count mismatch in {run_id}")
        trials.append(report)
        provenance.append({
            "run_id": run_id,
            "started_at": report["started_at"],
            "finished_at": report["finished_at"],
            "report_sha256": digest(report_path),
            "results_sha256": digest(result_path),
            "samples_sha256": digest(samples_path),
            "docker_stats_sha256": digest(stats_path),
            "samples": sample_count,
        })

    groups: dict[tuple[str, int], list[dict]] = {}
    for trial in trials:
        for cell in trial["cells"]:
            groups.setdefault((cell["phase"], int(cell["concurrency"])), []).append(cell)
    cells = []
    for (phase, concurrency), rows in sorted(groups.items()):
        if len(rows) != len(trials):
            raise ValueError(f"incomplete cell {phase}/{concurrency}")
        summary = {
            "phase": phase,
            "concurrency": concurrency,
            "samples_per_trial": int(median(rows, ("samples",))),
            "latency_ms": {key: median(rows, ("latency_ms", key)) for key in ("p50", "p95", "p99", "mean")},
            "p95_trial_range_ms": [min(row["latency_ms"]["p95"] for row in rows), max(row["latency_ms"]["p95"] for row in rows)],
            "throughput_qps": median(rows, ("throughput_qps",)),
        }
        if all("database_ms" in row for row in rows):
            summary["database_ms"] = {key: median(rows, ("database_ms", key)) for key in ("p50", "p95", "mean")}
        component_names = sorted(set.intersection(*(set(row.get("component_ms", {})) for row in rows)))
        if component_names:
            summary["component_ms"] = {
                name: {key: median(rows, ("component_ms", name, key)) for key in ("p50", "p95", "mean")}
                for name in component_names
            }
        if all("fact_history_hit_rate" in row for row in rows):
            summary["fact_history_hit_rate"] = median(rows, ("fact_history_hit_rate",))
            summary["query_history_hit_rate"] = median(rows, ("query_history_hit_rate",))
        if all("lock_contention" in row for row in rows):
            summary["lock_contention"] = {
                key: median(rows, ("lock_contention", key))
                for key in ("samples", "samples_with_waiters", "max_waiting_sessions", "waiting_session_ms_approx")
            }
        if all("ledger_growth" in row for row in rows):
            summary["ledger_growth"] = {
                key: median(rows, ("ledger_growth", key))
                for key in ("release_used", "influence_used", "fact_rows", "fact_payload_bytes", "table_bytes", "indexes_bytes")
            }
        cells.append(summary)

    by_key = {(cell["phase"], cell["concurrency"]): cell for cell in cells}
    overhead = []
    for concurrency in (1, 4, 8):
        direct = by_key[("business_sql", concurrency)]
        full = by_key[("full_history_hit", concurrency)]
        overhead.append({
            "concurrency": concurrency,
            "p50_latency_ratio": full["latency_ms"]["p50"] / direct["latency_ms"]["p50"],
            "p95_latency_ratio": full["latency_ms"]["p95"] / direct["latency_ms"]["p95"],
            "throughput_ratio": full["throughput_qps"] / direct["throughput_qps"],
        })

    full_path_per_trial = sum(cell["samples"] for cell in trials[0]["cells"] if cell["phase"].startswith("full_history_"))
    total = sum(item["samples"] for item in provenance)
    full_path = full_path_per_trial * len(trials)
    environment_path = HERE / "environment.json"
    return {
        "schema_version": 2,
        "status": "complete_controlled_local_campaign",
        "campaign_id": "rq4-local-postgresql-20260728",
        "trials": len(trials),
        "observations": total,
        "operation_partition": {"full_path": full_path, "ablations": total - full_path, "total": total},
        "aggregation": "median of three independent trial summaries; per-trial percentiles use Hyndman-Fan type 7",
        "configuration": expected_configuration,
        "environment": json.loads(environment_path.read_text(encoding="utf-8")),
        "environment_sha256": digest(environment_path),
        "gateway_benchmark_source_sha256": tree_digest(SOURCE_PATHS),
        "raw_provenance": provenance,
        "cells": cells,
        "full_vs_direct": overhead,
        "service_peak_memory_bytes": {
            service: statistics.median(trial["service_peak_memory_bytes"][service] for trial in trials)
            for service in ("gateway", "control-postgres", "business-postgres")
        },
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", nargs="+", default=DEFAULT_RUNS)
    parser.add_argument("--output", type=Path, default=HERE / "results.json")
    parser.add_argument("--check", action="store_true", help="verify that --output exactly matches the raw campaign")
    args = parser.parse_args()
    try:
        result = build_summary(args.runs)
    except (KeyError, TypeError, ValueError, json.JSONDecodeError) as error:
        raise SystemExit(f"RQ4 raw campaign validation failed: {error}") from error
    if args.check:
        published = json.loads(args.output.read_text(encoding="utf-8"))
        if published != result:
            raise SystemExit(f"RQ4 published summary is not reproducible from raw samples: {args.output}")
        print(f"ok - reproduced {result['observations']} observations from {len(args.runs)} raw trials")
        return
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    output_label = args.output.resolve().relative_to(ROOT).as_posix()
    print(f"wrote {output_label}: {result['observations']} observations ({result['operation_partition']['full_path']} full-path)")


if __name__ == "__main__":
    main()
