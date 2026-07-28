#!/usr/bin/env python3
"""Validate the workflow-study design and, optionally, collected evidence."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import re
from collections import defaultdict
from pathlib import Path


HERE = Path(__file__).resolve().parent
TASKS = HERE / "tasks.json"
PROTOCOL = HERE / "protocol.json"
TASK_ID = re.compile(r"^(FIN|SUP|PROC)-[0-9]{2}$")
ARMS = {"taskgate_v3", "query_count", "returned_rows", "serialized_bytes"}
BUDGET_FIELDS = {
    "taskgate_v3": {"release_facts", "influence_facts", "outcome_facts"},
    "query_count": {"successful_queries"},
    "returned_rows": {"returned_rows"},
    "serialized_bytes": {"serialized_bytes"},
}


def load(path: Path) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ValueError(f"cannot read JSON {path}: {error}") from error


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def timestamp(value: object, label: str) -> dt.datetime:
    require(isinstance(value, str) and value.strip(), f"{label} must be an RFC3339 timestamp")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise ValueError(f"{label} is not an RFC3339 timestamp") from error
    require(parsed.tzinfo is not None, f"{label} must include a timezone")
    return parsed


def validate_budget(value: object, arm: str, label: str) -> None:
    require(isinstance(value, dict), f"{label} must be an object")
    require(set(value) == BUDGET_FIELDS[arm], f"{label} has the wrong units for {arm}")
    require(
        all(isinstance(amount, int) and not isinstance(amount, bool) and amount >= 0 for amount in value.values()),
        f"{label} values must be non-negative integers",
    )


def validate_design() -> tuple[dict, dict]:
    tasks_doc = load(TASKS)
    protocol = load(PROTOCOL)
    require(tasks_doc.get("schema_version") == 1, "unsupported task manifest")
    require(protocol.get("schema_version") == 1, "unsupported study protocol")
    require(tasks_doc.get("task_set_id") == protocol.get("study_id"), "study identifiers differ")

    tasks = tasks_doc.get("tasks", [])
    require(len(tasks) == 12, "the registered design requires exactly 12 tasks")
    seen: set[str] = set()
    domains: dict[str, int] = defaultdict(int)
    vectors: set[tuple[int, ...]] = set()
    for task in tasks:
        task_id = task.get("id", "")
        require(TASK_ID.fullmatch(task_id) is not None, f"invalid task id {task_id!r}")
        require(task_id not in seen, f"duplicate task {task_id}")
        seen.add(task_id)
        domain = task.get("domain")
        require(domain in {"finance", "support", "procurement"}, f"invalid domain for {task_id}")
        domains[domain] += 1
        require(len(task.get("prompt", "").split()) >= 25, f"task {task_id} prompt is not a workflow request")
        products = task.get("products")
        columns = task.get("approved_columns")
        require(
            isinstance(products, list) and products and len(products) == len(set(products))
            and all(isinstance(product, str) and product for product in products),
            f"task {task_id} has invalid products",
        )
        require(isinstance(columns, dict) and set(columns) == set(products), f"task {task_id} columns do not match its products")
        require(
            all(
                isinstance(values, list) and values and len(values) == len(set(values))
                and all(isinstance(column, str) and column for column in values)
                for values in columns.values()
            ),
            f"task {task_id} has invalid approved columns",
        )
        scope = task.get("scope")
        require(
            isinstance(scope, dict) and set(scope) == {"business_unit", "event_date"},
            f"task {task_id} lacks the registered business-unit/date scope",
        )
        require(task.get("ground_truth_key") == task_id, f"task {task_id} ground-truth key differs")
        rubric = task.get("rubric", [])
        require(len(rubric) >= 4, f"task {task_id} rubric is too small")
        weights = [item.get("weight") for item in rubric]
        require(all(isinstance(weight, int) and weight > 0 for weight in weights), f"task {task_id} has invalid weights")
        require(sum(weights) == 100, f"task {task_id} rubric weights do not sum to 100")
        require(any(item.get("critical") is True for item in rubric), f"task {task_id} has no critical goal")
        item_ids = [item.get("id") for item in rubric]
        require(len(item_ids) == len(set(item_ids)) and all(item_ids), f"task {task_id} repeats a rubric item")
        for item in rubric:
            method = item.get("method", "")
            require(
                method in {
                    "exact", "set_f1", "ordered_list_overlap", "numeric_relative_2pct",
                    "numeric_absolute_0_01", "numeric_absolute_0_1", "blind_expert", "trace_guardrail",
                },
                f"task {task_id} uses unknown scoring method {method!r}",
            )
            if method not in {"blind_expert", "trace_guardrail"}:
                require(item.get("answer_path"), f"task {task_id} automated item lacks answer_path")
        vectors.add(tuple(weights))
    require(dict(domains) == {"finance": 4, "support": 4, "procurement": 4}, "domain balance differs from registration")
    require(len(vectors) >= 8, "rubric weights are insufficiently differentiated")

    protocol_arms = {arm.get("id") for arm in protocol.get("arms", [])}
    require(protocol_arms == ARMS, "registered policy arms differ")
    sampling = protocol.get("sampling", {})
    require(
        sampling.get("planned_agent_runs") == len(tasks) * len(ARMS) * sampling.get("agent_seeds_per_task_arm", 0),
        "planned run count is inconsistent",
    )
    require(
        sampling.get("independent_domain_experts")
        == sampling.get("budget_calibration_experts", 0) + sampling.get("approval_review_experts", 0),
        "registered calibration and approval panel counts are inconsistent",
    )
    require(sampling.get("budget_calibration_experts") == 9, "the design requires three calibrators per domain")
    require(sampling.get("approval_review_experts") == 9, "the design requires three approvers per domain")
    require(sampling.get("blind_grading_experts") == 6, "the design requires two blind graders per domain")
    require(protocol.get("status") == "designed_not_collected", "design file must not claim uncollected evidence")
    ground_truth_sql = (HERE / "db/10-ground-truth.sql").read_text(encoding="utf-8")
    registered_truth = set(re.findall(r"SELECT '((?:FIN|SUP|PROC)-[0-9]{2})'", ground_truth_sql))
    require(registered_truth == seen, "ground-truth SQL and task manifest differ")
    for template in ("expert-budget.example.json", "approval-review.example.json", "agent-run.example.json", "blind-grading.example.json"):
        require((HERE / "templates" / template).is_file(), f"missing collection template {template}")
    require((HERE / "controller.py").is_file(), "missing baseline buffer-before-release controller")
    unit_cards = load(HERE / "unit-cards.json")
    require({card.get("arm") for card in unit_cards.get("cards", [])} == ARMS, "unit cards do not cover all arms")
    return tasks_doc, protocol


def has_path(value: object, path: str) -> bool:
    current = value
    for part in path.split("."):
        if not isinstance(current, dict) or part not in current:
            return False
        current = current[part]
    return True


def validate_truth(path: Path, task_ids: set[str], tasks_doc: dict | None = None) -> None:
    truth = load(path)
    require(set(truth) == task_ids, "exported ground truth does not cover exactly the registered tasks")
    for task_id, answer in truth.items():
        require(isinstance(answer, dict) and answer, f"ground truth {task_id} is empty")
    if tasks_doc is not None:
        for task in tasks_doc["tasks"]:
            for item in task["rubric"]:
                if item["method"] not in {"blind_expert", "trace_guardrail"}:
                    require(
                        has_path(truth[task["id"]], item["answer_path"]),
                        f"ground truth {task['id']} lacks {item['answer_path']}",
                    )


def json_files(directory: Path) -> list[Path]:
    require(directory.is_dir(), f"collection directory is missing: {directory}")
    return sorted(path for path in directory.glob("*.json") if ".example." not in path.name)


def validate_budgets(
    directory: Path,
    task_ids: set[str],
    minimum_experts: int,
    registered_experts: int = 6,
    task_domains: dict[str, str] | None = None,
) -> None:
    coverage: dict[tuple[str, str], set[str]] = defaultdict(set)
    experts: set[str] = set()
    expert_domains: dict[str, str] = {}
    files = json_files(directory)
    require(files, "no independent expert budget records were supplied")
    for path in files:
        record = load(path)
        require(record.get("schema_version") == 1, f"unsupported expert record {path.name}")
        expert = record.get("expert_id", "")
        require(expert and "replace" not in expert, f"placeholder expert id in {path.name}")
        require(record.get("panel") == "calibration", f"wrong expert panel in {path.name}")
        require(record.get("is_paper_author") is False, f"paper author cannot supply independent budget in {path.name}")
        require(record.get("relevant_experience_years", -1) >= 1, f"expert experience is missing in {path.name}")
        timestamp(record.get("training_completed_at"), f"{path.name}.training_completed_at")
        domain = record.get("domain")
        require(domain in {"finance", "support", "procurement"}, f"invalid expert domain in {path.name}")
        require(expert_domains.get(expert, domain) == domain, f"expert {expert} appears in multiple domains")
        expert_domains[expert] = domain
        experts.add(expert)
        for calibration in record.get("calibrations", []):
            task_id, arm = calibration.get("task_id"), calibration.get("arm")
            require(task_id in task_ids and arm in ARMS, f"unknown task/arm in {path.name}")
            if task_domains is not None:
                require(task_domains[task_id] == domain, f"out-of-domain calibration in {path.name}")
            started = timestamp(calibration.get("started_at"), f"{path.name}.started_at")
            finished = timestamp(calibration.get("finished_at"), f"{path.name}.finished_at")
            require(finished > started, f"non-positive calibration duration in {path.name}")
            validate_budget(calibration.get("selected_budget"), arm, f"{path.name}.selected_budget")
            require(1 <= calibration.get("confidence_1_to_5", 0) <= 5, f"invalid confidence in {path.name}")
            require(isinstance(calibration.get("unit_comprehension_correct"), bool), f"missing comprehension check in {path.name}")
            coverage[(task_id, arm)].add(expert)
    require(len(experts) >= registered_experts, f"fewer than {registered_experts} independent experts supplied budgets")
    if task_domains is not None:
        for domain in {"finance", "support", "procurement"}:
            require(
                sum(value == domain for value in expert_domains.values()) >= minimum_experts,
                f"fewer than {minimum_experts} calibration experts for {domain}",
            )
    missing = [f"{task}/{arm}" for task in sorted(task_ids) for arm in sorted(ARMS) if len(coverage[(task, arm)]) < minimum_experts]
    require(not missing, "fewer than registered experts for: " + ", ".join(missing))


def validate_approvals(
    directory: Path,
    task_ids: set[str],
    minimum_experts: int,
    calibration_directory: Path | None = None,
    registered_experts: int = 6,
    task_domains: dict[str, str] | None = None,
) -> None:
    coverage: dict[tuple[str, str], set[str]] = defaultdict(set)
    experts: set[str] = set()
    expert_domains: dict[str, str] = {}
    calibration_experts: set[str] = set()
    if calibration_directory is not None:
        calibration_experts = {load(path).get("expert_id", "") for path in json_files(calibration_directory)}
    files = json_files(directory)
    require(files, "no independent approval-review records were supplied")
    for path in files:
        record = load(path)
        require(record.get("schema_version") == 1, f"unsupported approval record {path.name}")
        expert = record.get("expert_id", "")
        require(expert and "replace" not in expert, f"placeholder approval expert id in {path.name}")
        require(expert not in calibration_experts, f"expert panels overlap at {expert}")
        require(record.get("panel") == "approval", f"wrong approval panel in {path.name}")
        require(record.get("is_paper_author") is False, f"paper author cannot supply approval evidence in {path.name}")
        require(record.get("relevant_experience_years", -1) >= 1, f"approval expert experience is missing in {path.name}")
        timestamp(record.get("training_completed_at"), f"{path.name}.training_completed_at")
        domain = record.get("domain")
        require(domain in {"finance", "support", "procurement"}, f"invalid approval domain in {path.name}")
        require(expert_domains.get(expert, domain) == domain, f"approval expert {expert} appears in multiple domains")
        expert_domains[expert] = domain
        experts.add(expert)
        for decision in record.get("decisions", []):
            task_id, arm = decision.get("task_id"), decision.get("arm")
            require(task_id in task_ids and arm in ARMS, f"unknown approval task/arm in {path.name}")
            if task_domains is not None:
                require(task_domains[task_id] == domain, f"out-of-domain approval in {path.name}")
            rendered = timestamp(decision.get("rendered_at"), f"{path.name}.rendered_at")
            decided = timestamp(decision.get("decided_at"), f"{path.name}.decided_at")
            require(decided > rendered, f"non-positive approval duration in {path.name}")
            require(decision.get("decision") in {"approve", "reject", "narrow"}, f"invalid decision in {path.name}")
            validate_budget(decision.get("requested_budget"), arm, f"{path.name}.requested_budget")
            if decision.get("decision") != "reject":
                validate_budget(decision.get("approved_budget"), arm, f"{path.name}.approved_budget")
                require(
                    all(decision["approved_budget"][unit] <= amount for unit, amount in decision["requested_budget"].items()),
                    f"approved budget widens the request in {path.name}",
                )
                if decision.get("decision") == "approve":
                    require(decision["approved_budget"] == decision["requested_budget"], f"approve changes the budget in {path.name}")
                else:
                    require(decision["approved_budget"] != decision["requested_budget"], f"narrow leaves the budget unchanged in {path.name}")
            else:
                require(decision.get("approved_budget") in (None, {}), f"rejected request has an approved budget in {path.name}")
            require(1 <= decision.get("confidence_1_to_5", 0) <= 5, f"invalid approval confidence in {path.name}")
            require(isinstance(decision.get("unit_comprehension_correct"), bool), f"missing approval comprehension check in {path.name}")
            coverage[(task_id, arm)].add(expert)
    require(len(experts) >= registered_experts, f"fewer than {registered_experts} independent approval experts supplied decisions")
    if task_domains is not None:
        for domain in {"finance", "support", "procurement"}:
            require(
                sum(value == domain for value in expert_domains.values()) >= minimum_experts,
                f"fewer than {minimum_experts} approval experts for {domain}",
            )
    missing = [f"{task}/{arm}" for task in sorted(task_ids) for arm in sorted(ARMS) if len(coverage[(task, arm)]) < minimum_experts]
    require(not missing, "fewer than registered approval experts for: " + ", ".join(missing))


def validate_runs(directory: Path, task_ids: set[str], seeds: int) -> None:
    coverage: dict[tuple[str, str], set[int]] = defaultdict(set)
    run_ids: set[str] = set()
    files = json_files(directory)
    require(files, "no agent run records were supplied")
    for path in files:
        record = load(path)
        require(record.get("schema_version") == 1, f"unsupported run record {path.name}")
        run_id = record.get("run_id", "")
        require(run_id and "replace" not in run_id and run_id not in run_ids, f"invalid or duplicate run id in {path.name}")
        run_ids.add(run_id)
        task_id, arm, seed = record.get("task_id"), record.get("arm"), record.get("seed")
        require(task_id in task_ids and arm in ARMS, f"unknown task/arm in {path.name}")
        require(isinstance(seed, int) and 0 <= seed < seeds, f"invalid registered seed in {path.name}")
        require(record.get("database_snapshot") == "workflow-study-2026-v1", f"wrong snapshot in {path.name}")
        require(
            record.get("status") in {"completed", "approval_rejected", "budget_exhausted", "tool_error", "agent_error"},
            f"invalid status in {path.name}",
        )
        require(timestamp(record.get("finished_at"), f"{path.name}.finished_at") > timestamp(record.get("started_at"), f"{path.name}.started_at"), f"invalid run time in {path.name}")
        validate_budget(record.get("budget"), arm, f"{path.name}.budget")
        require(isinstance(record.get("queries"), list), f"queries are missing in {path.name}")
        require(isinstance(record.get("final_answer"), dict), f"final answer is missing in {path.name}")
        risk = record.get("common_v3_risk", {})
        require(all(isinstance(risk.get(key), int) and risk[key] >= 0 for key in ("release_facts", "influence_facts", "outcome_facts", "sensitivity_weighted_exposure")), f"invalid common risk in {path.name}")
        native = record.get("native_usage", {})
        require(
            all(
                isinstance(native.get(key), int) and not isinstance(native.get(key), bool) and native[key] >= 0
                for key in ("successful_queries", "returned_rows", "serialized_bytes")
            ),
            f"invalid native usage in {path.name}",
        )
        require(
            isinstance(record.get("runtime_budget_rejections"), int)
            and not isinstance(record.get("runtime_budget_rejections"), bool)
            and record["runtime_budget_rejections"] >= 0,
            f"invalid runtime rejection count in {path.name}",
        )
        coverage[(task_id, arm)].add(seed)
    missing = [f"{task}/{arm}" for task in sorted(task_ids) for arm in sorted(ARMS) if coverage[(task, arm)] != set(range(seeds))]
    require(not missing, "incomplete registered seed coverage for: " + ", ".join(missing))


def validate_gradings(
    directory: Path,
    runs_directory: Path,
    tasks_doc: dict,
    minimum_graders: int,
    registered_graders: int,
    calibration_directory: Path | None = None,
    approval_directory: Path | None = None,
) -> None:
    tasks = {task["id"]: task for task in tasks_doc["tasks"]}
    runs = {}
    for path in json_files(runs_directory):
        run = load(path)
        runs[run["run_id"]] = run
    covered: dict[str, set[str]] = defaultdict(set)
    graders: set[str] = set()
    grader_domains: dict[str, str] = {}
    excluded: set[str] = set()
    for panel_directory in (calibration_directory, approval_directory):
        if panel_directory is not None:
            excluded.update(load(path).get("expert_id", "") for path in json_files(panel_directory))
    for path in json_files(directory):
        record = load(path)
        require(record.get("schema_version") == 1, f"unsupported grading record {path.name}")
        run_id = record.get("run_id", "")
        require(run_id in runs, f"unknown graded run in {path.name}")
        grader = record.get("grader_id", "")
        require(grader and "replace" not in grader, f"placeholder grader id in {path.name}")
        require(grader not in excluded, f"grader panel overlaps another expert panel at {grader}")
        require(record.get("panel") == "blind_grading", f"wrong grading panel in {path.name}")
        require(record.get("is_paper_author") is False, f"paper author cannot grade runs in {path.name}")
        require(record.get("relevant_experience_years", -1) >= 1, f"grader experience is missing in {path.name}")
        require(record.get("arm_blinded") is True, f"grader was not arm-blinded in {path.name}")
        domain = record.get("domain")
        run = runs[run_id]
        require(domain == tasks[run["task_id"]]["domain"], f"out-of-domain grading in {path.name}")
        require(grader_domains.get(grader, domain) == domain, f"grader {grader} appears in multiple domains")
        grader_domains[grader] = domain
        require(grader not in covered[run_id], f"duplicate grader/run pair in {path.name}")
        manual_items = {
            item["id"]
            for item in tasks[run["task_id"]]["rubric"]
            if item["method"] in {"blind_expert", "trace_guardrail"}
        }
        scores = record.get("scores")
        require(isinstance(scores, dict) and set(scores) == manual_items, f"manual rubric coverage differs in {path.name}")
        require(
            all(
                isinstance(value, (int, float)) and not isinstance(value, bool) and 0 <= float(value) <= 1
                for value in scores.values()
            ),
            f"invalid manual score in {path.name}",
        )
        covered[run_id].add(grader)
        graders.add(grader)
    require(len(graders) >= registered_graders, f"fewer than {registered_graders} independent blind graders")
    for domain in {"finance", "support", "procurement"}:
        require(
            sum(value == domain for value in grader_domains.values()) >= minimum_graders,
            f"fewer than {minimum_graders} blind graders for {domain}",
        )
    missing = [
        run_id
        for run_id, run in sorted(runs.items())
        if run.get("status") == "completed" and len(covered[run_id]) < minimum_graders
    ]
    require(not missing, "completed runs lack registered blind grades: " + ", ".join(missing))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--truth", type=Path)
    parser.add_argument("--budgets", type=Path)
    parser.add_argument("--approvals", type=Path)
    parser.add_argument("--runs", type=Path)
    parser.add_argument("--gradings", type=Path)
    args = parser.parse_args()
    try:
        tasks_doc, protocol = validate_design()
        task_ids = {task["id"] for task in tasks_doc["tasks"]}
        task_domains = {task["id"]: task["domain"] for task in tasks_doc["tasks"]}
        sampling = protocol["sampling"]
        if args.truth:
            validate_truth(args.truth, task_ids, tasks_doc)
        if args.budgets:
            validate_budgets(
                args.budgets,
                task_ids,
                sampling["minimum_experts_per_task"],
                sampling["budget_calibration_experts"],
                task_domains,
            )
        if args.approvals:
            validate_approvals(
                args.approvals,
                task_ids,
                sampling["minimum_experts_per_task"],
                args.budgets,
                sampling["approval_review_experts"],
                task_domains,
            )
        if args.runs:
            validate_runs(args.runs, task_ids, sampling["agent_seeds_per_task_arm"])
        if args.gradings:
            require(args.runs is not None, "--gradings requires --runs")
            validate_gradings(
                args.gradings,
                args.runs,
                tasks_doc,
                sampling["minimum_blind_graders_per_completed_run"],
                sampling["blind_grading_experts"],
                args.budgets,
                args.approvals,
            )
    except ValueError as error:
        raise SystemExit(f"workflow-study validation failed: {error}") from error
    print(
        f"ok - workflow-study design: {len(tasks_doc['tasks'])} tasks, "
        f"{len(protocol['arms'])} arms, {protocol['sampling']['planned_agent_runs']} planned runs"
    )


if __name__ == "__main__":
    main()
