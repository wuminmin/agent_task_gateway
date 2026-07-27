#!/usr/bin/env python3
"""Validate and summarize repeated full-path exposure performance trials."""

from __future__ import annotations

import argparse
import hashlib
import json
import statistics
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
DEFAULT_RUNS = [f"rq4-20260727-trial{index}" for index in range(1, 4)]


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def tree_digest(paths: list[Path]) -> str:
    checksum = hashlib.sha256()
    files: list[Path] = []
    for path in paths:
        files.extend(path.rglob("*.go") if path.is_dir() else [path])
    for path in sorted(files):
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


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs", nargs="+", default=DEFAULT_RUNS)
    parser.add_argument("--output", type=Path, default=Path(__file__).with_name("results.json"))
    args = parser.parse_args()

    raw_root = Path(__file__).with_name("raw")
    trials = []
    provenance = []
    expected_configuration = None
    for run_id in args.runs:
        run_dir = raw_root / run_id
        result_path = run_dir / "results.json"
        samples_path = run_dir / "samples.jsonl"
        report = json.loads(result_path.read_text(encoding="utf-8"))
        if report.get("schema_version") != 1 or report.get("status") != "smoke":
            raise SystemExit(f"invalid raw trial {run_id}")
        configuration = report["configuration"]
        comparable = {key: configuration[key] for key in (
            "cache_strategy", "concurrency", "ramp_runs", "runs_per_worker",
            "task_concurrency_mode", "workload",
        )}
        if expected_configuration is None:
            expected_configuration = comparable
        elif comparable != expected_configuration:
            raise SystemExit(f"configuration mismatch in {run_id}")
        if comparable != {
            "cache_strategy": "warm", "concurrency": [1, 4, 8], "ramp_runs": 32,
            "runs_per_worker": 200, "task_concurrency_mode": "delegated_tasks_shared_root",
            "workload": "expense_detail/sales/ordered/limit-1",
        }:
            raise SystemExit(f"unexpected publication configuration in {run_id}: {comparable}")
        sample_count = sum(1 for line in samples_path.read_text(encoding="utf-8").splitlines() if line.strip())
        if sample_count != 10432 or sum(cell["samples"] for cell in report["cells"]) != sample_count:
            raise SystemExit(f"sample count mismatch in {run_id}")
        if len(report["cells"]) != 13:
            raise SystemExit(f"cell count mismatch in {run_id}")
        for cell in report["cells"]:
            if cell["phase"] == "full_history_hit":
                if cell.get("fact_history_hit_rate") != 1 or cell.get("query_history_hit_rate") != 1:
                    raise SystemExit(f"history-hit cell is not a complete hit in {run_id}")
                if any(cell["ledger_growth"][key] != 0 for key in ("release_used", "influence_used", "fact_rows")):
                    raise SystemExit(f"history-hit cell grew the ledger in {run_id}")
        trials.append(report)
        provenance.append({
            "run_id": run_id,
            "started_at": report["started_at"],
            "finished_at": report["finished_at"],
            "results_sha256": digest(result_path),
            "samples_sha256": digest(samples_path),
            "samples": sample_count,
        })

    groups: dict[tuple[str, int], list[dict]] = {}
    for trial in trials:
        for cell in trial["cells"]:
            groups.setdefault((cell["phase"], int(cell["concurrency"])), []).append(cell)
    cells = []
    for (phase, concurrency), rows in sorted(groups.items()):
        if len(rows) != len(trials):
            raise SystemExit(f"incomplete cell {phase}/{concurrency}")
        summary = {
            "phase": phase,
            "concurrency": concurrency,
            "samples_per_trial": int(median(rows, ("samples",))),
            "latency_ms": {
                key: median(rows, ("latency_ms", key))
                for key in ("p50", "p95", "p99", "mean")
            },
            "p95_trial_range_ms": [
                min(row["latency_ms"]["p95"] for row in rows),
                max(row["latency_ms"]["p95"] for row in rows),
            ],
            "throughput_qps": median(rows, ("throughput_qps",)),
        }
        if all("database_ms" in row for row in rows):
            summary["database_ms"] = {
                key: median(rows, ("database_ms", key)) for key in ("p50", "p95", "mean")
            }
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

    environment_path = Path(__file__).with_name("environment.json")
    result = {
        "schema_version": 1,
        "status": "complete_controlled_local_campaign",
        "campaign_id": "rq4-local-postgresql-20260727",
        "trials": len(trials),
        "observations": sum(item["samples"] for item in provenance),
        "aggregation": "median of three independent trial summaries; per-trial percentiles use Hyndman-Fan type 7",
        "configuration": expected_configuration,
        "environment": json.loads(environment_path.read_text(encoding="utf-8")),
        "environment_sha256": digest(environment_path),
        "gateway_benchmark_source_sha256": tree_digest([
            ROOT / "internal", ROOT / "cmd/gateway", ROOT / "evaluation/cmd/exposure-bench",
            ROOT / "compose.yaml", Path(__file__).with_name("compose.yaml"), Path(__file__).with_name("catalog.yaml"),
        ]),
        "raw_provenance": provenance,
        "cells": cells,
        "full_vs_direct": overhead,
        "service_peak_memory_bytes": {
            service: statistics.median(trial["service_peak_memory_bytes"][service] for trial in trials)
            for service in ("gateway", "control-postgres", "business-postgres")
        },
    }
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    output_label = args.output.resolve().relative_to(ROOT).as_posix()
    print(f"wrote {output_label}: {result['observations']} observations")


if __name__ == "__main__":
    main()
