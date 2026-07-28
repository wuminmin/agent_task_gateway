#!/usr/bin/env python3
"""Freeze deterministic domain budgets from held-out unlimited calibration runs."""

from __future__ import annotations

import argparse
import json
import math
import tempfile
from collections import defaultdict
from pathlib import Path

import validate


def lower_median(values: list[int]) -> int:
    if not values:
        raise ValueError("cannot take the lower median of an empty sample")
    ordered = sorted(values)
    return ordered[(len(ordered) - 1) // 2]


def nested_value(record: dict, path: str) -> int:
    current: object = record
    for component in path.split("."):
        if not isinstance(current, dict) or component not in current:
            raise ValueError(f"calibration record lacks registered usage path {path}")
        current = current[component]
    if not isinstance(current, int) or isinstance(current, bool) or current < 0:
        raise ValueError(f"calibration usage at {path} is not a nonnegative integer")
    return current


def aggregate(records: list[dict], calibration_doc: dict) -> dict:
    task_domains = {task["id"]: task["domain"] for task in calibration_doc["tasks"]}
    by_domain: dict[str, list[dict]] = defaultdict(list)
    for record in records:
        by_domain[task_domains[record["task_id"]]].append(record)

    domains = {}
    for domain in sorted(validate.DOMAINS):
        traces = by_domain[domain]
        if len(traces) != 6:
            raise ValueError(f"{domain} must contribute exactly six completed calibration traces")
        base = {}
        for arm in sorted(validate.PRIMARY_ARMS):
            base[arm] = {
                unit: lower_median([nested_value(record, path) for record in traces])
                for unit, path in sorted(validate.USAGE_UNIT_MAPPING[arm].items())
            }
        levels = {
            validate.level_key(level): {
                arm: {
                    unit: max(1, math.floor(amount * level))
                    for unit, amount in base[arm].items()
                }
                for arm in sorted(validate.PRIMARY_ARMS)
            }
            for level in validate.BUDGET_LEVELS
        }
        domains[domain] = {
            "completed_calibration_runs": len(traces),
            "base_statistic": "component-wise lower median over two held-out tasks times three replicates",
            "base": base,
            "levels": levels,
        }
    return domains


def build_freeze(
    calibration_runs: Path,
    execution_lock: Path,
    frozen_at: str,
) -> dict:
    tasks_doc, calibration_doc, protocol = validate.validate_design()
    del tasks_doc
    frozen_time = validate.timestamp(frozen_at, "frozen_at")
    lock = validate.validate_execution_lock(execution_lock, protocol["study_id"])
    records = validate.validate_calibration_runs(
        calibration_runs,
        calibration_doc,
        protocol,
        validate.file_sha256(execution_lock),
        lock,
    )
    latest_calibration_finish = max(
        validate.timestamp(record["finished_at"], f"{record['run_id']}.finished_at")
        for record in records
    )
    if frozen_time <= latest_calibration_finish:
        raise ValueError("frozen_at must be later than every calibration run")
    payload = {
        "schema_version": 2,
        "study_id": protocol["study_id"],
        "status": "frozen_from_held_out_calibration",
        "frozen_at": frozen_at,
        "evaluation_task_manifest_sha256": validate.file_sha256(validate.TASKS),
        "calibration_task_manifest_sha256": validate.file_sha256(validate.CALIBRATION_TASKS),
        "protocol_sha256": validate.file_sha256(validate.PROTOCOL),
        "execution_lock_sha256": validate.file_sha256(execution_lock),
        "source_file_sha256": validate.source_file_digests(),
        "calibration_run_records": validate.record_digests(calibration_runs),
        "usage_unit_mapping": validate.USAGE_UNIT_MAPPING,
        "levels": list(validate.BUDGET_LEVELS),
        "level_rule": "max(1, floor(domain_lower_median_usage * level)) component-wise",
        "domains": aggregate(records, calibration_doc),
    }
    payload["freeze_sha256"] = validate.canonical_sha256(payload)
    return payload


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--calibration-runs", required=True, type=Path)
    parser.add_argument("--execution-lock", required=True, type=Path)
    parser.add_argument("--frozen-at", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    try:
        payload = build_freeze(args.calibration_runs, args.execution_lock, args.frozen_at)
        _write(args.output, payload)
        validate.validate_algorithmic_freeze(
            args.output,
            validate.load(validate.PROTOCOL),
            args.execution_lock,
            args.calibration_runs,
        )
    except ValueError as error:
        raise SystemExit(f"budget freeze failed: {error}") from error
    print(f"wrote algorithmic budget freeze: {args.output} ({payload['freeze_sha256']})")


def _write(path: Path, payload: dict) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=path.parent, delete=False) as temporary:
        json.dump(payload, temporary, ensure_ascii=False, indent=2, sort_keys=True)
        temporary.write("\n")
        temporary_path = Path(temporary.name)
    temporary_path.replace(path)
    return path


if __name__ == "__main__":
    main()
