#!/usr/bin/env python3
"""Analyze the two preregistered experiments without manufacturing evidence."""

from __future__ import annotations

import argparse
import csv
import datetime as dt
import hashlib
import itertools
import json
import math
import random
import statistics
from collections import defaultdict
from pathlib import Path

import validate


def load(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def path_value(value: object, path: str) -> object:
    current = value
    for part in path.split("."):
        if not isinstance(current, dict) or part not in current:
            return None
        current = current[part]
    return current


def number(value: object) -> float | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, (int, float)) and math.isfinite(float(value)):
        return float(value)
    if isinstance(value, str):
        try:
            parsed = float(value)
        except ValueError:
            return None
        return parsed if math.isfinite(parsed) else None
    return None


def set_metrics(actual: object, expected: object) -> tuple[float, float, float]:
    if not isinstance(actual, list) or not isinstance(expected, list):
        return 0.0, 0.0, 0.0
    left = {json.dumps(item, ensure_ascii=False, sort_keys=True) for item in actual}
    right = {json.dumps(item, ensure_ascii=False, sort_keys=True) for item in expected}
    if not left and not right:
        return 1.0, 1.0, 1.0
    precision = len(left & right) / len(left) if left else 0.0
    recall = len(left & right) / len(right) if right else 0.0
    f1 = 2 * precision * recall / (precision + recall) if precision + recall else 0.0
    return precision, recall, f1


def set_f1(actual: object, expected: object) -> float:
    return set_metrics(actual, expected)[2]


def automated_score(method: str, actual: object, expected: object) -> float:
    if method == "exact":
        return float(actual == expected)
    if method == "set_f1":
        return set_f1(actual, expected)
    if method == "ordered_list_overlap":
        if not isinstance(actual, list) or not isinstance(expected, list):
            return 0.0
        if not expected:
            return float(not actual)
        correct = sum(1 for index, item in enumerate(expected) if index < len(actual) and actual[index] == item)
        return correct / len(expected)
    actual_number, expected_number = number(actual), number(expected)
    if actual_number is None or expected_number is None:
        return 0.0
    difference = abs(actual_number - expected_number)
    if method == "numeric_absolute_0_01":
        return float(difference <= 0.01)
    if method == "numeric_absolute_0_1":
        return float(difference <= 0.1)
    if method == "numeric_relative_2pct":
        return max(0.0, 1.0 - difference / (0.02 * max(abs(expected_number), 1e-9)))
    raise ValueError(f"unsupported scoring method {method}")


def percentile(values: list[float], probability: float) -> float:
    ordered = sorted(values)
    if not ordered:
        return 0.0
    position = (len(ordered) - 1) * probability
    lower, upper = math.floor(position), math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def cluster_interval(rows: list[dict], field: str, seed: int = 20260728) -> list[float]:
    by_task: dict[str, list[float]] = defaultdict(list)
    for row in rows:
        if row.get(field) is not None:
            by_task[row["task_id"]].append(float(row[field]))
    tasks = sorted(by_task)
    if not tasks:
        return [0.0, 0.0]
    generator = random.Random(seed)
    draws: list[float] = []
    for _ in range(2000):
        selected = [generator.choice(tasks) for _ in tasks]
        values = [value for task in selected for value in by_task[task]]
        draws.append(statistics.fmean(values))
    return [percentile(draws, 0.025), percentile(draws, 0.975)]


def paired_differences(rows: list[dict], baseline: str, field: str) -> list[dict]:
    indexed = {(row["task_id"], row["seed"], row["arm"]): row for row in rows}
    differences = []
    for task_id, seed, _ in sorted(key for key in indexed if key[2] == "taskgate_v3"):
        other = indexed.get((task_id, seed, baseline))
        if other is None:
            raise ValueError(f"missing paired {baseline} run for {task_id}/seed-{seed}")
        differences.append({
            "task_id": task_id,
            "seed": seed,
            "difference": float(indexed[(task_id, seed, "taskgate_v3")][field]) - float(other[field]),
        })
    return differences


def exact_task_sign_flip_p(rows: list[dict]) -> float:
    by_task: dict[str, list[float]] = defaultdict(list)
    for row in rows:
        by_task[row["task_id"]].append(row["difference"])
    task_means = [statistics.fmean(by_task[task]) for task in sorted(by_task)]
    observed = abs(statistics.fmean(task_means))
    extreme = 0
    assignments = 0
    for signs in itertools.product((-1, 1), repeat=len(task_means)):
        assignments += 1
        statistic = abs(statistics.fmean(sign * value for sign, value in zip(signs, task_means)))
        extreme += statistic >= observed - 1e-12
    return extreme / assignments


def holm_adjust(pvalues: dict[str, float]) -> dict[str, float]:
    ordered = sorted(pvalues, key=pvalues.get)
    adjusted: dict[str, float] = {}
    running = 0.0
    for index, name in enumerate(ordered):
        running = max(running, min(1.0, (len(ordered) - index) * pvalues[name]))
        adjusted[name] = running
    return adjusted


def grade_map(directory: Path) -> dict[str, dict]:
    scores: dict[str, dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    counts: dict[str, dict[str, list[int]]] = defaultdict(lambda: defaultdict(list))
    for path in validate.json_files(directory):
        record = load(path)
        if record.get("arm_blinded") is not True:
            raise ValueError(f"grader was not arm-blinded in {path.name}")
        run_id = record.get("run_id", "")
        for item, value in record.get("scores", {}).items():
            if not isinstance(value, (int, float)) or not 0 <= float(value) <= 1:
                raise ValueError(f"invalid manual score {item} in {path.name}")
            scores[run_id][item].append(float(value))
        for field in ("unsupported_claim_count", "factual_error_count"):
            counts[run_id][field].append(int(record[field]))
    return {
        run_id: {
            "scores": {item: statistics.fmean(values) for item, values in scores[run_id].items()},
            **{field: statistics.fmean(values) for field, values in counts[run_id].items()},
        }
        for run_id in scores.keys() | counts.keys()
    }


def mean_or_none(values: list[float]) -> float | None:
    return statistics.fmean(values) if values else None


def score_run(run: dict, task: dict, truth: dict, manual: dict) -> dict:
    risk = run["common_v3_risk"]
    native = run["native_usage"]
    performance = run["performance"]
    base = {
        "run_id": run["run_id"], "task_id": task["id"], "domain": task["domain"],
        "difficulty": task["difficulty"], "arm": run["arm"], "seed": run["seed"],
        "phase": run["phase"], "budget_multiplier": float(run["budget_multiplier"]),
        "status": run["status"], "runtime_budget_rejections": run.get("runtime_budget_rejections", 0),
        **risk, **native, **performance,
    }
    if run.get("status") != "completed":
        return {
            **base, "rubric_score": 0.0, "task_complete": 0.0,
            "numeric_absolute_error": None, "numeric_relative_error": None,
            "list_precision": None, "list_recall": None,
            "unsupported_claim_count": 0.0, "factual_error_count": 0.0,
            "item_scores": {},
        }
    item_scores: dict[str, float] = {}
    absolute_errors: list[float] = []
    relative_errors: list[float] = []
    precisions: list[float] = []
    recalls: list[float] = []
    manual_scores = manual.get("scores", {})
    for item in task["rubric"]:
        method = item["method"]
        if method in {"blind_expert", "trace_guardrail"}:
            if item["id"] not in manual_scores:
                raise ValueError(f"run {run['run_id']} lacks blinded score {item['id']}")
            score = manual_scores[item["id"]]
        else:
            actual = path_value(run.get("final_answer", {}), item["answer_path"])
            expected = path_value(truth, item["answer_path"])
            score = automated_score(method, actual, expected)
            if method.startswith("numeric_"):
                left, right = number(actual), number(expected)
                if left is not None and right is not None:
                    absolute_errors.append(abs(left - right))
                    relative_errors.append(abs(left - right) / max(abs(right), 1e-9))
            if method == "set_f1":
                precision, recall, _ = set_metrics(actual, expected)
                precisions.append(precision)
                recalls.append(recall)
        item_scores[item["id"]] = min(1.0, max(0.0, score))
    total = sum(item["weight"] * item_scores[item["id"]] for item in task["rubric"])
    critical = all(item_scores[item["id"]] >= 0.8 for item in task["rubric"] if item.get("critical"))
    return {
        **base, "rubric_score": total, "task_complete": float(total >= 80 and critical),
        "numeric_absolute_error": mean_or_none(absolute_errors),
        "numeric_relative_error": mean_or_none(relative_errors),
        "list_precision": mean_or_none(precisions), "list_recall": mean_or_none(recalls),
        "unsupported_claim_count": manual.get("unsupported_claim_count", 0.0),
        "factual_error_count": manual.get("factual_error_count", 0.0), "item_scores": item_scores,
    }


def summarize_runs(rows: list[dict]) -> dict:
    result = {"runs": len(rows)}
    for field in (
        "rubric_score", "task_complete", "numeric_absolute_error", "numeric_relative_error",
        "list_precision", "list_recall", "unsupported_claim_count", "factual_error_count",
        "release_facts", "influence_facts", "outcome_facts", "sensitivity_weighted_exposure",
        "distinct_sensitive_records", "distinct_sensitive_fields", "unnecessary_sensitive_fields",
        "successful_queries", "returned_rows", "serialized_bytes", "wall_clock_ms",
        "gateway_latency_ms", "accounting_latency_ms", "exposure_storage_bytes",
    ):
        values = [float(row[field]) for row in rows if row.get(field) is not None]
        result[f"mean_{field}"] = statistics.fmean(values) if values else None
    result["rubric_score_task_cluster_ci95"] = cluster_interval(rows, "rubric_score")
    result["completion_task_cluster_ci95"] = cluster_interval(rows, "task_complete")
    result["runtime_budget_rejections"] = sum(row["runtime_budget_rejections"] for row in rows)
    result["final_task_failures"] = sum(row["status"] != "completed" for row in rows)
    return result


def usability_rows(directory: Path) -> list[dict]:
    rows = []
    for path in validate.json_files(directory):
        record = load(path)
        for decision in record["decisions"]:
            rendered = dt.datetime.fromisoformat(decision["rendered_at"].replace("Z", "+00:00"))
            decided = dt.datetime.fromisoformat(decision["decided_at"].replace("Z", "+00:00"))
            rows.append({
                "task_id": decision["task_id"], "domain": record["domain"], "expert_id": record["expert_id"],
                "arm": decision["arm"], "decision": decision["decision"],
                "decision_seconds": (decided - rendered).total_seconds(),
                "rejected": float(decision["decision"] == "reject"),
                "narrowed": float(decision["decision"] == "narrow"),
                "budget_edit_count": decision["budget_edit_count"],
                "comprehension_score": decision["comprehension"]["correct"] / decision["comprehension"]["total"],
                "confidence": decision["confidence_1_to_5"],
            })
    return rows


def usability_summary(rows: list[dict]) -> dict:
    result = {}
    for arm in sorted(validate.PRIMARY_ARMS):
        selected = [row for row in rows if row["arm"] == arm]
        non_rejected = [row for row in selected if row["decision"] != "reject"]
        result[arm] = {
            "reviews": len(selected),
            "median_decision_seconds": statistics.median(row["decision_seconds"] for row in selected),
            "decision_seconds_task_cluster_ci95": cluster_interval(selected, "decision_seconds"),
            "approve_rate": statistics.fmean(row["decision"] == "approve" for row in selected),
            "initial_rejection_rate": statistics.fmean(row["rejected"] for row in selected),
            "rejection_rate_task_cluster_ci95": cluster_interval(selected, "rejected"),
            "narrowing_rate_given_non_rejection": statistics.fmean(row["narrowed"] for row in non_rejected) if non_rejected else None,
            "mean_budget_edit_count": statistics.fmean(row["budget_edit_count"] for row in selected),
            "mean_comprehension_score": statistics.fmean(row["comprehension_score"] for row in selected),
            "mean_confidence_1_to_5": statistics.fmean(row["confidence"] for row in selected),
        }
    return result


def pareto_frontiers(rows: list[dict]) -> dict:
    candidates = [
        row for row in rows
        if row["phase"] == "pareto_sweep" or (
            row["phase"] == "primary" and row["seed"] == 0 and row["budget_multiplier"] == 1.0
        )
    ]
    output = {}
    for arm in sorted(validate.PRIMARY_ARMS):
        points = []
        for multiplier in (0.5, 0.75, 1.0, 1.25):
            selected = [row for row in candidates if row["arm"] == arm and row["budget_multiplier"] == multiplier]
            points.append({
                "budget_multiplier": multiplier,
                "mean_rubric_score": statistics.fmean(row["rubric_score"] for row in selected),
                "mean_sensitivity_weighted_exposure": statistics.fmean(row["sensitivity_weighted_exposure"] for row in selected),
                "rubric_score_task_cluster_ci95": cluster_interval(selected, "rubric_score"),
                "exposure_task_cluster_ci95": cluster_interval(selected, "sensitivity_weighted_exposure"),
            })
        for point in points:
            point["pareto_efficient"] = not any(
                other is not point
                and other["mean_rubric_score"] >= point["mean_rubric_score"]
                and other["mean_sensitivity_weighted_exposure"] <= point["mean_sensitivity_weighted_exposure"]
                and (
                    other["mean_rubric_score"] > point["mean_rubric_score"]
                    or other["mean_sensitivity_weighted_exposure"] < point["mean_sensitivity_weighted_exposure"]
                )
                for other in points
            )
        output[arm] = points
    return output


def write_scored_csv(path: Path, rows: list[dict]) -> None:
    fields = [key for key in rows[0] if key != "item_scores"]
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields, extrasaction="ignore")
        writer.writeheader()
        writer.writerows(rows)


def write_rows_csv(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--truth", required=True, type=Path)
    parser.add_argument("--task-reviews", required=True, type=Path)
    parser.add_argument("--budgets", required=True, type=Path)
    parser.add_argument("--freeze", required=True, type=Path)
    parser.add_argument("--execution-lock", required=True, type=Path)
    parser.add_argument("--approvals", required=True, type=Path)
    parser.add_argument("--runs", required=True, type=Path)
    parser.add_argument("--gradings", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--scored-csv", type=Path)
    parser.add_argument("--decisions-csv", type=Path)
    args = parser.parse_args()
    try:
        tasks_doc, protocol = validate.validate_design()
        tasks = {task["id"]: task for task in tasks_doc["tasks"]}
        task_ids = set(tasks)
        task_domains = {task_id: task["domain"] for task_id, task in tasks.items()}
        sampling = protocol["sampling"]
        validate.validate_truth(args.truth, task_ids, tasks_doc)
        validate.validate_task_reviews(args.task_reviews, tasks_doc)
        validate.validate_budgets(args.budgets, task_ids, sampling["minimum_experts_per_task"], sampling["budget_calibration_experts"], task_domains)
        lock = validate.validate_execution_lock(args.execution_lock, protocol["study_id"])
        frozen = validate.validate_frozen(args.freeze, protocol, args.execution_lock, args.task_reviews, args.budgets)
        claimed = frozen["freeze_sha256"]
        lock_sha = hashlib.sha256(args.execution_lock.read_bytes()).hexdigest()
        validate.validate_approvals(args.approvals, task_ids, sampling["minimum_experts_per_task"], args.budgets, sampling["budget_usability_experts"], task_domains, frozen)
        validate.validate_runs(args.runs, task_ids, sampling, frozen, lock_sha)
        validate.validate_gradings(args.gradings, args.runs, tasks_doc, sampling["minimum_blind_graders_per_completed_run"], sampling["blind_grading_experts"], args.budgets, args.approvals)

        truth = load(args.truth)
        manual = grade_map(args.gradings)
        scored = [
            score_run(run, tasks[run["task_id"]], truth[run["task_id"]], manual.get(run["run_id"], {}))
            for run in (load(path) for path in validate.json_files(args.runs))
        ]
        primary = [row for row in scored if row["phase"] == "primary"]
        arm_summaries = {arm: summarize_runs([row for row in primary if row["arm"] == arm]) for arm in sorted(validate.PRIMARY_ARMS)}
        contrasts = {}
        pvalues = {}
        for baseline in sorted(validate.BASELINE_ARMS):
            rubric = paired_differences(primary, baseline, "rubric_score")
            completion = paired_differences(primary, baseline, "task_complete")
            exposure = paired_differences(primary, baseline, "sensitivity_weighted_exposure")
            pvalues[baseline] = exact_task_sign_flip_p(rubric)
            contrasts[baseline] = {
                "paired_runs": len(rubric),
                "taskgate_minus_baseline_rubric_score": statistics.fmean(row["difference"] for row in rubric),
                "rubric_score_task_cluster_ci95": cluster_interval(rubric, "difference"),
                "rubric_score_exact_task_sign_flip_p": pvalues[baseline],
                "taskgate_minus_baseline_completion_rate": statistics.fmean(row["difference"] for row in completion),
                "completion_exact_task_sign_flip_p": exact_task_sign_flip_p(completion),
                "taskgate_minus_baseline_sensitivity_weighted_exposure": statistics.fmean(row["difference"] for row in exposure),
                "exposure_task_cluster_ci95": cluster_interval(exposure, "difference"),
            }
        for baseline, value in holm_adjust(pvalues).items():
            contrasts[baseline]["rubric_score_holm_adjusted_p"] = value
        unlimited = [row for row in scored if row["phase"] == "unlimited_upper_bound"]
        decision_rows = usability_rows(args.approvals)
        report = {
            "schema_version": 2,
            "study_id": protocol["study_id"],
            "status": "complete_registered_collection",
            "execution_lock": {"provider": lock["provider"], "model": lock["model"], "model_version": lock["model_version"]},
            "budget_freeze_sha256": claimed,
            "experiments_are_independent": True,
            "experiment_a_agent_utility": {
                "primary_metric": protocol["metrics"]["primary"]["id"],
                "primary_runs": len(primary), "primary_arms": arm_summaries,
                "paired_taskgate_contrasts": contrasts,
                "unlimited_upper_bound": summarize_runs(unlimited),
                "utility_risk_pareto": pareto_frontiers(scored),
            },
            "experiment_b_expert_budget_usability": {
                "decisions_do_not_gate_agent_runs": True,
                "arms": usability_summary(decision_rows),
                "calibration_agreement": frozen["agreement"],
            },
            "scored_runs": scored,
        }
    except (ValueError, KeyError, TypeError, ZeroDivisionError) as error:
        raise SystemExit(f"workflow-study analysis failed: {error}") from error
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    if args.scored_csv:
        write_scored_csv(args.scored_csv, scored)
    if args.decisions_csv:
        write_rows_csv(args.decisions_csv, decision_rows)
    print(f"wrote {args.output}: {len(scored)} scored runs")


if __name__ == "__main__":
    main()
