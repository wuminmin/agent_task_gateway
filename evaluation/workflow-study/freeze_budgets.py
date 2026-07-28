#!/usr/bin/env python3
"""Freeze pre-run expert budgets and agreement statistics into one digest."""

from __future__ import annotations

import argparse
import hashlib
import json
from collections import defaultdict
from pathlib import Path

import validate


def file_sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def record_set(directory: Path) -> list[dict[str, str]]:
    return [{"name": path.name, "sha256": file_sha256(path)} for path in validate.json_files(directory)]


def lower_median(values: list[int]) -> int:
    ordered = sorted(values)
    return ordered[(len(ordered) - 1) // 2]


def average_ranks(values: list[int]) -> list[float]:
    ordered = sorted(range(len(values)), key=values.__getitem__)
    result = [0.0] * len(values)
    position = 0
    while position < len(ordered):
        end = position + 1
        while end < len(ordered) and values[ordered[end]] == values[ordered[position]]:
            end += 1
        rank = ((position + 1) + end) / 2
        for index in ordered[position:end]:
            result[index] = rank
        position = end
    return result


def kendalls_w(matrix: list[list[int]]) -> float | None:
    if len(matrix) < 2 or not matrix[0] or any(len(row) != len(matrix[0]) for row in matrix):
        return None
    raters, items = len(matrix), len(matrix[0])
    ranks = [average_ranks(row) for row in matrix]
    rank_sums = [sum(row[item] for row in ranks) for item in range(items)]
    center = raters * (items + 1) / 2
    numerator = 12 * sum((value - center) ** 2 for value in rank_sums)
    tie_correction = 0
    for row in matrix:
        counts: dict[int, int] = defaultdict(int)
        for value in row:
            counts[value] += 1
        tie_correction += sum(count**3 - count for count in counts.values())
    denominator = raters**2 * (items**3 - items) - raters * tie_correction
    return round(numerator / denominator, 6) if denominator else None


def collect(directory: Path, tasks: dict[str, dict]) -> tuple[dict, dict]:
    values: dict[tuple[str, str, str], list[int]] = defaultdict(list)
    by_expert: dict[tuple[str, str, str, str], int] = {}
    for path in validate.json_files(directory):
        record = validate.load(path)
        expert = record["expert_id"]
        for item in record["calibrations"]:
            for unit, amount in item["selected_budget"].items():
                values[(item["task_id"], item["arm"], unit)].append(amount)
                by_expert[(expert, item["task_id"], item["arm"], unit)] = amount

    frozen: dict[str, dict] = {}
    cell_agreement = []
    for task_id in sorted(tasks):
        frozen[task_id] = {}
        for arm in sorted(validate.PRIMARY_ARMS):
            budget = {}
            for unit in sorted(validate.BUDGET_FIELDS[arm]):
                observed = values[(task_id, arm, unit)]
                median = lower_median(observed)
                deviations = [abs(value - median) for value in observed]
                mad = lower_median(deviations)
                budget[unit] = median
                cell_agreement.append(
                    {
                        "task_id": task_id,
                        "arm": arm,
                        "unit": unit,
                        "experts": len(observed),
                        "lower_median": median,
                        "mad": mad,
                        "relative_mad": round(mad / max(median, 1), 6),
                        "exact_agreement": len(set(observed)) == 1,
                    }
                )
            frozen[task_id][arm] = budget

    rank_agreement = []
    for domain in sorted(validate.DOMAINS):
        domain_tasks = sorted(task_id for task_id, task in tasks.items() if task["domain"] == domain)
        experts = sorted(
            {
                expert
                for expert, task_id, _, _ in by_expert
                if task_id in domain_tasks
            }
        )
        for arm in sorted(validate.PRIMARY_ARMS):
            for unit in sorted(validate.BUDGET_FIELDS[arm]):
                matrix = [[by_expert[(expert, task_id, arm, unit)] for task_id in domain_tasks] for expert in experts]
                rank_agreement.append(
                    {"domain": domain, "arm": arm, "unit": unit, "experts": len(experts), "kendalls_w": kendalls_w(matrix)}
                )
    return frozen, {"cells": cell_agreement, "rank_agreement": rank_agreement}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--task-reviews", required=True, type=Path)
    parser.add_argument("--budgets", required=True, type=Path)
    parser.add_argument("--execution-lock", required=True, type=Path)
    parser.add_argument("--frozen-at", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    tasks_doc, protocol = validate.validate_design()
    tasks = {task["id"]: task for task in tasks_doc["tasks"]}
    task_ids = set(tasks)
    task_domains = {task_id: task["domain"] for task_id, task in tasks.items()}
    sampling = protocol["sampling"]
    validate.timestamp(args.frozen_at, "frozen_at")
    validate.validate_task_reviews(args.task_reviews, tasks_doc)
    validate.validate_budgets(
        args.budgets,
        task_ids,
        sampling["minimum_experts_per_task"],
        sampling["budget_calibration_experts"],
        task_domains,
    )
    validate.validate_execution_lock(args.execution_lock, protocol["study_id"])

    budgets, agreement = collect(args.budgets, tasks)
    payload = {
        "schema_version": 1,
        "study_id": protocol["study_id"],
        "status": "frozen_before_agent_runs",
        "frozen_at": args.frozen_at,
        "display_context_sha256": validate.display_context_sha256(),
        "task_manifest_sha256": file_sha256(validate.TASKS),
        "protocol_sha256": file_sha256(validate.PROTOCOL),
        "task_review_records": record_set(args.task_reviews),
        "budget_records": record_set(args.budgets),
        "execution_lock_sha256": file_sha256(args.execution_lock),
        "budgets": budgets,
        "agreement": agreement,
    }
    payload["freeze_sha256"] = validate.canonical_sha256(payload)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"wrote frozen budgets: {args.output} ({payload['freeze_sha256']})")


if __name__ == "__main__":
    main()
