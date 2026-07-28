#!/usr/bin/env python3
"""Score collected workflow-study records without manufacturing missing evidence."""

from __future__ import annotations

import argparse
import datetime as dt
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


def set_f1(actual: object, expected: object) -> float:
    if not isinstance(actual, list) or not isinstance(expected, list):
        return 0.0
    left = {json.dumps(item, ensure_ascii=False, sort_keys=True) for item in actual}
    right = {json.dumps(item, ensure_ascii=False, sort_keys=True) for item in expected}
    if not left and not right:
        return 1.0
    if not left or not right:
        return 0.0
    overlap = len(left & right)
    precision, recall = overlap / len(left), overlap / len(right)
    return 2 * precision * recall / (precision + recall) if precision + recall else 0.0


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
        scale = max(abs(expected_number), 1e-9)
        return max(0.0, 1.0 - difference / (0.02 * scale))
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
    taskgate_keys = sorted(key for key in indexed if key[2] == "taskgate_v3")
    for task_id, seed, _ in taskgate_keys:
        other = indexed.get((task_id, seed, baseline))
        if other is None:
            raise ValueError(f"missing paired {baseline} run for {task_id}/seed-{seed}")
        differences.append(
            {
                "task_id": task_id,
                "seed": seed,
                "difference": float(indexed[(task_id, seed, "taskgate_v3")][field]) - float(other[field]),
            }
        )
    return differences


def exact_task_sign_flip_p(rows: list[dict]) -> float:
    by_task: dict[str, list[float]] = defaultdict(list)
    for row in rows:
        by_task[row["task_id"]].append(row["difference"])
    task_means = [statistics.fmean(by_task[task]) for task in sorted(by_task)]
    observed = abs(statistics.fmean(task_means))
    assignments = 0
    at_least_as_extreme = 0
    for signs in itertools.product((-1, 1), repeat=len(task_means)):
        assignments += 1
        statistic = abs(statistics.fmean(sign * value for sign, value in zip(signs, task_means)))
        if statistic >= observed - 1e-12:
            at_least_as_extreme += 1
    return at_least_as_extreme / assignments


def holm_adjust(pvalues: dict[str, float]) -> dict[str, float]:
    ordered = sorted(pvalues, key=pvalues.get)
    adjusted: dict[str, float] = {}
    running = 0.0
    total = len(ordered)
    for index, name in enumerate(ordered):
        running = max(running, min(1.0, (total - index) * pvalues[name]))
        adjusted[name] = running
    return adjusted


def grade_map(directory: Path) -> dict[str, dict[str, float]]:
    raw: dict[str, dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    for path in validate.json_files(directory):
        record = load(path)
        if record.get("arm_blinded") is not True:
            raise ValueError(f"grader was not arm-blinded in {path.name}")
        for item, value in record.get("scores", {}).items():
            if not isinstance(value, (int, float)) or not 0 <= float(value) <= 1:
                raise ValueError(f"invalid manual score {item} in {path.name}")
            raw[record.get("run_id", "")][item].append(float(value))
    return {
        run_id: {item: statistics.fmean(values) for item, values in items.items()}
        for run_id, items in raw.items()
    }


def score_run(run: dict, task: dict, truth: dict, manual: dict[str, float]) -> dict:
    if run.get("status") != "completed":
        return {"run_id": run["run_id"], "task_id": task["id"], "arm": run["arm"], "seed": run["seed"],
                "rubric_score": 0.0, "task_complete": 0.0,
                "sensitivity_weighted_exposure": run["common_v3_risk"]["sensitivity_weighted_exposure"],
                "runtime_budget_rejections": run.get("runtime_budget_rejections", 0)}
    scores: dict[str, float] = {}
    for item in task["rubric"]:
        method = item["method"]
        if method in {"blind_expert", "trace_guardrail"}:
            if item["id"] not in manual:
                raise ValueError(f"run {run['run_id']} lacks blinded score {item['id']}")
            score = manual[item["id"]]
        else:
            actual = path_value(run.get("final_answer", {}), item["answer_path"])
            expected = path_value(truth, item["answer_path"])
            score = automated_score(method, actual, expected)
        scores[item["id"]] = min(1.0, max(0.0, score))
    total = sum(item["weight"] * scores[item["id"]] for item in task["rubric"])
    critical = all(scores[item["id"]] >= 0.8 for item in task["rubric"] if item.get("critical"))
    return {"run_id": run["run_id"], "task_id": task["id"], "arm": run["arm"], "seed": run["seed"],
            "rubric_score": total, "task_complete": float(total >= 80 and critical),
            "sensitivity_weighted_exposure": run["common_v3_risk"]["sensitivity_weighted_exposure"],
            "runtime_budget_rejections": run.get("runtime_budget_rejections", 0), "item_scores": scores}


def approval_summary(directory: Path) -> dict:
    rows = []
    for path in validate.json_files(directory):
        record = load(path)
        for decision in record.get("decisions", []):
            rendered = dt.datetime.fromisoformat(decision["rendered_at"].replace("Z", "+00:00"))
            decided = dt.datetime.fromisoformat(decision["decided_at"].replace("Z", "+00:00"))
            rows.append(
                {
                    "task_id": decision["task_id"],
                    "arm": decision["arm"],
                    "decision": decision["decision"],
                    "seconds": (decided - rendered).total_seconds(),
                    "rejected": float(decision["decision"] == "reject"),
                    "narrowed": float(decision["decision"] == "narrow"),
                }
            )
    result = {}
    for arm in sorted(validate.ARMS):
        selected = [row for row in rows if row["arm"] == arm]
        non_rejected = [row for row in selected if row["decision"] != "reject"]
        result[arm] = {
            "reviews": len(selected),
            "median_decision_seconds": statistics.median(row["seconds"] for row in selected),
            "mean_decision_seconds_task_cluster_ci95": cluster_interval(selected, "seconds"),
            "initial_rejection_rate": sum(row["decision"] == "reject" for row in selected) / len(selected),
            "rejection_rate_task_cluster_ci95": cluster_interval(selected, "rejected"),
            "narrowing_rate_given_non_rejection": (
                sum(row["decision"] == "narrow" for row in non_rejected) / len(non_rejected)
                if non_rejected else None
            ),
            "narrowing_rate_task_cluster_ci95": cluster_interval(non_rejected, "narrowed") if non_rejected else None,
        }
    return result


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--truth", required=True, type=Path)
    parser.add_argument("--budgets", required=True, type=Path)
    parser.add_argument("--approvals", required=True, type=Path)
    parser.add_argument("--runs", required=True, type=Path)
    parser.add_argument("--gradings", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        tasks_doc, protocol = validate.validate_design()
        task_ids = {task["id"] for task in tasks_doc["tasks"]}
        task_domains = {task["id"]: task["domain"] for task in tasks_doc["tasks"]}
        sampling = protocol["sampling"]
        validate.validate_truth(args.truth, task_ids, tasks_doc)
        validate.validate_budgets(
            args.budgets,
            task_ids,
            sampling["minimum_experts_per_task"],
            sampling["budget_calibration_experts"],
            task_domains,
        )
        validate.validate_approvals(
            args.approvals,
            task_ids,
            sampling["minimum_experts_per_task"],
            args.budgets,
            sampling["approval_review_experts"],
            task_domains,
        )
        validate.validate_runs(args.runs, task_ids, sampling["agent_seeds_per_task_arm"])
        validate.validate_gradings(
            args.gradings,
            args.runs,
            tasks_doc,
            sampling["minimum_blind_graders_per_completed_run"],
            sampling["blind_grading_experts"],
            args.budgets,
            args.approvals,
        )
        truth = load(args.truth)
        tasks = {task["id"]: task for task in tasks_doc["tasks"]}
        manual = grade_map(args.gradings)
        scored = []
        for path in validate.json_files(args.runs):
            run = load(path)
            scored.append(
                score_run(
                    run,
                    tasks[run["task_id"]],
                    truth[run["task_id"]],
                    manual.get(run["run_id"], {}),
                )
            )
        arms = {}
        for arm in sorted(validate.ARMS):
            rows = [row for row in scored if row["arm"] == arm]
            arms[arm] = {
                "runs": len(rows),
                "mean_rubric_score": statistics.fmean(row["rubric_score"] for row in rows),
                "rubric_score_task_cluster_ci95": cluster_interval(rows, "rubric_score"),
                "task_completion_rate": statistics.fmean(row["task_complete"] for row in rows),
                "completion_task_cluster_ci95": cluster_interval(rows, "task_complete"),
                "mean_sensitivity_weighted_exposure": statistics.fmean(row["sensitivity_weighted_exposure"] for row in rows),
                "runtime_budget_rejections": sum(row["runtime_budget_rejections"] for row in rows),
            }
        contrasts = {}
        rubric_pvalues = {}
        for baseline in sorted(validate.ARMS - {"taskgate_v3"}):
            rubric = paired_differences(scored, baseline, "rubric_score")
            completion = paired_differences(scored, baseline, "task_complete")
            exposure = paired_differences(scored, baseline, "sensitivity_weighted_exposure")
            rubric_pvalues[baseline] = exact_task_sign_flip_p(rubric)
            contrasts[baseline] = {
                "paired_runs": len(rubric),
                "taskgate_minus_baseline_rubric_score": statistics.fmean(row["difference"] for row in rubric),
                "rubric_score_task_cluster_ci95": cluster_interval(rubric, "difference"),
                "rubric_score_exact_task_sign_flip_p": rubric_pvalues[baseline],
                "taskgate_minus_baseline_completion_rate": statistics.fmean(row["difference"] for row in completion),
                "completion_task_cluster_ci95": cluster_interval(completion, "difference"),
                "completion_exact_task_sign_flip_p": exact_task_sign_flip_p(completion),
                "taskgate_minus_baseline_sensitivity_weighted_exposure": statistics.fmean(
                    row["difference"] for row in exposure
                ),
                "exposure_task_cluster_ci95": cluster_interval(exposure, "difference"),
            }
        adjusted = holm_adjust(rubric_pvalues)
        for baseline, value in adjusted.items():
            contrasts[baseline]["rubric_score_holm_adjusted_p"] = value
        report = {
            "schema_version": 1,
            "study_id": protocol["study_id"],
            "status": "complete_collected_workflow_study",
            "tasks": len(tasks),
            "runs": len(scored),
            "arms": arms,
            "paired_taskgate_contrasts": contrasts,
            "approval": approval_summary(args.approvals),
            "scored_runs": scored,
        }
    except (ValueError, KeyError, TypeError, ZeroDivisionError) as error:
        raise SystemExit(f"workflow-study analysis failed: {error}") from error
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"wrote {args.output}: {len(scored)} scored runs")


if __name__ == "__main__":
    main()
