#!/usr/bin/env python3
"""Validate the workflow-study design and, optionally, collected evidence."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import re
from collections import defaultdict
from pathlib import Path


HERE = Path(__file__).resolve().parent
TASKS = HERE / "tasks.json"
PROTOCOL = HERE / "protocol.json"
TASK_ID = re.compile(r"^(FIN|SUP|PROC)-[0-9]{2}$")
DOMAINS = {"finance", "risk_compliance", "customer_operations"}
PRIMARY_ARMS = {"taskgate_v3", "query_count", "returned_rows", "serialized_bytes"}
BASELINE_ARMS = PRIMARY_ARMS - {"taskgate_v3"}
ARMS = PRIMARY_ARMS | {"unlimited"}
BUDGET_FIELDS = {
    "taskgate_v3": {"release_facts", "influence_facts", "outcome_facts"},
    "query_count": {"successful_queries"},
    "returned_rows": {"returned_rows"},
    "serialized_bytes": {"serialized_bytes"},
    "unlimited": set(),
}
BUDGET_MAX = {
    "taskgate_v3": {"release_facts": 100000, "influence_facts": 1000000, "outcome_facts": 100},
    "query_count": {"successful_queries": 100},
    "returned_rows": {"returned_rows": 5000},
    "serialized_bytes": {"serialized_bytes": 5000000},
    "unlimited": {},
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
    if arm == "taskgate_v3":
        require(all(amount > 0 for amount in value.values()), f"{label} TaskGate ceilings must be positive")
    require(
        all(amount <= BUDGET_MAX[arm][unit] for unit, amount in value.items()),
        f"{label} exceeds the registered operational range for {arm}",
    )


def canonical_sha256(value: object) -> str:
    encoded = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def display_context() -> dict:
    tasks_doc = load(TASKS)
    cards = load(HERE / "unit-cards.json")
    return {
        "study_id": tasks_doc["task_set_id"],
        "tasks": [
            {
                key: task[key]
                for key in (
                    "id", "domain", "difficulty", "label", "prompt", "products",
                    "approved_columns", "scope", "sensitivity",
                )
            }
            for task in tasks_doc["tasks"]
        ],
        "unit_cards": [card for card in cards["cards"] if card["arm"] in PRIMARY_ARMS],
        "risk_preference": load(HERE / "risk-preference-card.json"),
        # The collection UI renders catalog.yaml. Binding its bytes here prevents
        # changing the visible schema after experts have selected ceilings.
        "catalog_sha256": hashlib.sha256((HERE / "catalog.yaml").read_bytes()).hexdigest(),
    }


def display_context_sha256() -> str:
    return canonical_sha256(display_context())


def file_sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def record_digests(directory: Path) -> list[dict[str, str]]:
    return [{"name": path.name, "sha256": file_sha256(path)} for path in json_files(directory)]


def validate_execution_lock(path: Path, study_id: str) -> dict:
    lock = load(path)
    require(lock.get("schema_version") == 1 and lock.get("study_id") == study_id, "invalid execution lock identity")
    for field in ("provider", "model", "model_version"):
        require(isinstance(lock.get(field), str) and lock[field] and "replace" not in lock[field], f"invalid execution lock {field}")
    require(
        isinstance(lock.get("temperature"), (int, float)) and not isinstance(lock.get("temperature"), bool),
        "invalid execution lock temperature",
    )
    for field in ("system_prompt_sha256", "tool_surface_sha256", "agent_adapter_sha256", "answer_schema_sha256"):
        require(re.fullmatch(r"[0-9a-f]{64}", lock.get(field, "")) is not None, f"invalid execution lock {field}")
    return lock


def validate_frozen(
    path: Path,
    protocol: dict,
    execution_lock: Path,
    task_reviews: Path | None = None,
    budgets: Path | None = None,
) -> dict:
    frozen = load(path)
    claimed = frozen.get("freeze_sha256", "")
    payload = dict(frozen)
    payload.pop("freeze_sha256", None)
    require(claimed == canonical_sha256(payload), "budget freeze digest mismatch")
    require(
        frozen.get("status") == "frozen_before_agent_runs" and frozen.get("study_id") == protocol["study_id"],
        "invalid budget freeze identity",
    )
    require(frozen.get("task_manifest_sha256") == file_sha256(TASKS), "task manifest differs from budget freeze")
    require(frozen.get("protocol_sha256") == file_sha256(PROTOCOL), "protocol differs from budget freeze")
    require(frozen.get("display_context_sha256") == display_context_sha256(), "expert context differs from budget freeze")
    validate_execution_lock(execution_lock, protocol["study_id"])
    require(frozen.get("execution_lock_sha256") == file_sha256(execution_lock), "execution lock differs from budget freeze")
    if task_reviews is not None:
        require(frozen.get("task_review_records") == record_digests(task_reviews), "task reviews differ from budget freeze")
    if budgets is not None:
        require(frozen.get("budget_records") == record_digests(budgets), "expert budgets differ from budget freeze")
    return frozen


def validate_design() -> tuple[dict, dict]:
    tasks_doc = load(TASKS)
    protocol = load(PROTOCOL)
    require(tasks_doc.get("schema_version") == 1, "unsupported task manifest")
    require(protocol.get("schema_version") == 2, "unsupported study protocol")
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
        require(domain in DOMAINS, f"invalid domain for {task_id}")
        domains[domain] += 1
        require(task.get("difficulty") in {"low", "medium", "high"}, f"task {task_id} lacks registered difficulty")
        require(
            task.get("case_source") == "representative_synthetic_case_subject_to_practitioner_provenance_gate",
            f"task {task_id} overstates or omits its current provenance",
        )
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
        require(4 <= len(rubric) <= 8, f"task {task_id} must have four to eight rubric goals")
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
    require(dict(domains) == {domain: 4 for domain in DOMAINS}, "domain balance differs from registration")
    require(len(vectors) >= 8, "rubric weights are insufficiently differentiated")

    protocol_arms = {arm.get("id") for arm in protocol.get("arms", [])}
    require(protocol_arms == ARMS, "registered policy arms differ")
    sampling = protocol.get("sampling", {})
    expected_primary = len(tasks) * len(PRIMARY_ARMS) * sampling.get("agent_seeds_per_primary_arm", 0)
    expected_unlimited = len(tasks) * sampling.get("agent_seeds_unlimited", 0)
    expected_pareto = (
        len(tasks) * len(PRIMARY_ARMS) * len(sampling.get("pareto_budget_multipliers", []))
        * len(sampling.get("pareto_sweep_seeds", []))
    )
    require(sampling.get("primary_agent_runs") == expected_primary, "primary run count is inconsistent")
    require(sampling.get("unlimited_diagnostic_runs") == expected_unlimited, "unlimited run count is inconsistent")
    require(sampling.get("pareto_sensitivity_runs") == expected_pareto, "Pareto run count is inconsistent")
    require(sampling.get("planned_agent_runs") == expected_primary + expected_unlimited + expected_pareto, "planned run count is inconsistent")
    require(sampling.get("budget_calibration_experts") == 9, "the design requires three calibrators per domain")
    require(sampling.get("budget_usability_experts") == 9, "the design requires three usability experts per domain")
    require(sampling.get("blind_grading_experts") == 6, "the design requires two blind graders per domain")
    require(
        sampling.get("seed_semantics")
        == "0--4 are paired replicate labels supplied in the task context; the DeepSeek API exposes no deterministic sampling-seed parameter",
        "replicate labels must not be overstated as provider-controlled random seeds",
    )
    require(
        sampling.get("agent_seeds_per_primary_arm") == sampling.get("agent_seeds_unlimited"),
        "the cyclic five-arm order requires equal primary and unlimited seed counts",
    )
    require(protocol.get("status") == "designed_not_collected", "design file must not claim uncollected evidence")
    require(
        protocol.get("experiments", {}).get("agent_utility", {}).get("independence")
        == "approval-usability decisions never suppress, relabel, or zero-score an agent run",
        "the two experiments are not causally separated",
    )
    ground_truth_sql = (HERE / "db/10-ground-truth.sql").read_text(encoding="utf-8")
    registered_truth = set(re.findall(r"SELECT '((?:FIN|SUP|PROC)-[0-9]{2})'", ground_truth_sql))
    require(registered_truth == seen, "ground-truth SQL and task manifest differ")
    for template in (
        "task-review.example.json", "expert-budget.example.json", "approval-review.example.json",
        "agent-run.example.json", "blind-grading.example.json",
    ):
        require((HERE / "templates" / template).is_file(), f"missing collection template {template}")
    require((HERE / "risk-preference-card.json").is_file(), "missing organization risk-preference card")
    require((HERE / "controller.py").is_file(), "missing baseline buffer-before-release controller")
    require((HERE / "system-prompt.txt").is_file(), "missing frozen Agent system prompt")
    require((HERE / "agent-tool-surface.json").is_file(), "missing frozen Agent tool surface")
    essentials = load(HERE / "essential-columns.json")
    require(essentials.get("study_id") == tasks_doc["task_set_id"], "essential-column study identity differs")
    require(set(essentials.get("tasks", {})) == seen, "essential columns do not cover exactly the task set")
    by_id = {task["id"]: task for task in tasks}
    for task_id, products in essentials["tasks"].items():
        require(set(products).issubset(by_id[task_id]["approved_columns"]), f"essential product exceeds grant for {task_id}")
        for product, fields in products.items():
            require(
                set(fields).issubset(by_id[task_id]["approved_columns"][product]),
                f"essential columns exceed grant for {task_id}/{product}",
            )
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


def validate_task_reviews(directory: Path, tasks_doc: dict, minimum_reviewers: int = 2) -> None:
    tasks = {task["id"]: task for task in tasks_doc["tasks"]}
    coverage: dict[str, set[str]] = defaultdict(set)
    reviewer_domains: dict[str, str] = {}
    contributions: dict[str, dict[str, set[str]]] = defaultdict(lambda: defaultdict(set))
    seen_assignments: set[tuple[str, str]] = set()
    files = json_files(directory)
    require(files, "no independent practitioner task reviews were supplied")
    for path in files:
        record = load(path)
        require(record.get("schema_version") == 1, f"unsupported task-review record {path.name}")
        reviewer = record.get("reviewer_id", "")
        require(reviewer and "replace" not in reviewer, f"placeholder task reviewer in {path.name}")
        require(record.get("is_paper_author") is False, f"paper author cannot validate task realism in {path.name}")
        require(record.get("relevant_experience_years", -1) >= 1, f"task reviewer experience is missing in {path.name}")
        domain = record.get("domain")
        require(domain in DOMAINS, f"invalid task-review domain in {path.name}")
        require(reviewer_domains.get(reviewer, domain) == domain, f"task reviewer {reviewer} appears in multiple domains")
        reviewer_domains[reviewer] = domain
        for review in record.get("reviews", []):
            task_id = review.get("task_id")
            require(task_id in tasks and tasks[task_id]["domain"] == domain, f"out-of-domain task review in {path.name}")
            assignment = (reviewer, task_id)
            require(assignment not in seen_assignments, f"duplicate practitioner/task review in {path.name}")
            seen_assignments.add(assignment)
            contribution = review.get("contribution")
            require(
                contribution in {"authored_or_substantively_adapted", "independent_validation"},
                f"invalid practitioner contribution in {path.name}",
            )
            require(review.get("decision") == "accept", f"task {task_id} still requires practitioner revision")
            require(1 <= review.get("realism_1_to_5", 0) <= 5, f"invalid realism rating in {path.name}")
            require(review.get("difficulty") in {"low", "medium", "high"}, f"invalid reviewed difficulty in {path.name}")
            timestamp(review.get("reviewed_at"), f"{path.name}.reviewed_at")
            coverage[task_id].add(reviewer)
            contributions[task_id][contribution].add(reviewer)
    missing = [task for task in sorted(tasks) if len(coverage[task]) < minimum_reviewers]
    require(not missing, "tasks lack two independent practitioner acceptances: " + ", ".join(missing))
    missing_origin = [
        task for task in sorted(tasks)
        if not contributions[task]["authored_or_substantively_adapted"]
        or not contributions[task]["independent_validation"]
        or contributions[task]["authored_or_substantively_adapted"] == contributions[task]["independent_validation"]
    ]
    require(
        not missing_origin,
        "tasks lack separate practitioner authorship/adaptation and independent validation: " + ", ".join(missing_origin),
    )
    for domain in DOMAINS:
        require(
            sum(value == domain for value in reviewer_domains.values()) >= minimum_reviewers,
            f"fewer than {minimum_reviewers} task reviewers for {domain}",
        )


def validate_budgets(
    directory: Path,
    task_ids: set[str],
    minimum_experts: int,
    registered_experts: int = 6,
    task_domains: dict[str, str] | None = None,
) -> None:
    coverage: dict[tuple[str, str], set[str]] = defaultdict(set)
    seen_assignments: set[tuple[str, str, str]] = set()
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
        require(record.get("display_context_sha256") == display_context_sha256(), f"display context drift in {path.name}")
        domain = record.get("domain")
        require(domain in DOMAINS, f"invalid expert domain in {path.name}")
        require(expert_domains.get(expert, domain) == domain, f"expert {expert} appears in multiple domains")
        expert_domains[expert] = domain
        experts.add(expert)
        for calibration in record.get("calibrations", []):
            task_id, arm = calibration.get("task_id"), calibration.get("arm")
            require(task_id in task_ids and arm in PRIMARY_ARMS, f"unknown or non-calibrated task/arm in {path.name}")
            assignment = (expert, task_id, arm)
            require(assignment not in seen_assignments, f"duplicate expert calibration in {path.name}")
            seen_assignments.add(assignment)
            if task_domains is not None:
                require(task_domains[task_id] == domain, f"out-of-domain calibration in {path.name}")
            started = timestamp(calibration.get("started_at"), f"{path.name}.started_at")
            finished = timestamp(calibration.get("finished_at"), f"{path.name}.finished_at")
            require(finished > started, f"non-positive calibration duration in {path.name}")
            validate_budget(calibration.get("selected_budget"), arm, f"{path.name}.selected_budget")
            require(1 <= calibration.get("confidence_1_to_5", 0) <= 5, f"invalid confidence in {path.name}")
            comprehension = calibration.get("comprehension", {})
            require(
                isinstance(comprehension.get("correct"), int)
                and isinstance(comprehension.get("total"), int)
                and 0 <= comprehension["correct"] <= comprehension["total"]
                and comprehension["total"] >= 2,
                f"invalid comprehension result in {path.name}",
            )
            coverage[(task_id, arm)].add(expert)
    require(len(experts) == registered_experts, f"expected exactly {registered_experts} independent calibration experts")
    if task_domains is not None:
        for domain in DOMAINS:
            require(
                sum(value == domain for value in expert_domains.values()) >= minimum_experts,
                f"fewer than {minimum_experts} calibration experts for {domain}",
            )
    wrong = [f"{task}/{arm}" for task in sorted(task_ids) for arm in sorted(PRIMARY_ARMS) if len(coverage[(task, arm)]) != minimum_experts]
    require(not wrong, "calibration coverage differs from registration for: " + ", ".join(wrong))


def validate_approvals(
    directory: Path,
    task_ids: set[str],
    minimum_experts: int,
    calibration_directory: Path | None = None,
    registered_experts: int = 6,
    task_domains: dict[str, str] | None = None,
    frozen: dict | None = None,
) -> None:
    coverage: dict[tuple[str, str], set[str]] = defaultdict(set)
    seen_assignments: set[tuple[str, str, str]] = set()
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
        require(record.get("panel") == "budget_usability", f"wrong budget-usability panel in {path.name}")
        require(record.get("is_paper_author") is False, f"paper author cannot supply approval evidence in {path.name}")
        require(record.get("relevant_experience_years", -1) >= 1, f"approval expert experience is missing in {path.name}")
        timestamp(record.get("training_completed_at"), f"{path.name}.training_completed_at")
        require(record.get("display_context_sha256") == display_context_sha256(), f"display context drift in {path.name}")
        domain = record.get("domain")
        require(domain in DOMAINS, f"invalid approval domain in {path.name}")
        require(expert_domains.get(expert, domain) == domain, f"approval expert {expert} appears in multiple domains")
        expert_domains[expert] = domain
        experts.add(expert)
        for decision in record.get("decisions", []):
            task_id, arm = decision.get("task_id"), decision.get("arm")
            require(task_id in task_ids and arm in PRIMARY_ARMS, f"unknown or non-reviewed task/arm in {path.name}")
            assignment = (expert, task_id, arm)
            require(assignment not in seen_assignments, f"duplicate expert usability decision in {path.name}")
            seen_assignments.add(assignment)
            if task_domains is not None:
                require(task_domains[task_id] == domain, f"out-of-domain approval in {path.name}")
            rendered = timestamp(decision.get("rendered_at"), f"{path.name}.rendered_at")
            decided = timestamp(decision.get("decided_at"), f"{path.name}.decided_at")
            require(decided > rendered, f"non-positive approval duration in {path.name}")
            require(decision.get("decision") in {"approve", "reject", "narrow"}, f"invalid decision in {path.name}")
            validate_budget(decision.get("requested_budget"), arm, f"{path.name}.requested_budget")
            if frozen is not None:
                require(
                    decision["requested_budget"] == frozen["budgets"][task_id][arm],
                    f"usability panel did not review the frozen request in {path.name}",
                )
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
            require(
                isinstance(decision.get("budget_edit_count"), int)
                and not isinstance(decision.get("budget_edit_count"), bool)
                and decision["budget_edit_count"] >= 0,
                f"invalid budget edit count in {path.name}",
            )
            comprehension = decision.get("comprehension", {})
            require(
                isinstance(comprehension.get("correct"), int)
                and isinstance(comprehension.get("total"), int)
                and 0 <= comprehension["correct"] <= comprehension["total"]
                and comprehension["total"] >= 2,
                f"invalid comprehension result in {path.name}",
            )
            coverage[(task_id, arm)].add(expert)
    require(len(experts) == registered_experts, f"expected exactly {registered_experts} independent usability experts")
    if task_domains is not None:
        for domain in DOMAINS:
            require(
                sum(value == domain for value in expert_domains.values()) >= minimum_experts,
                f"fewer than {minimum_experts} approval experts for {domain}",
            )
    wrong = [f"{task}/{arm}" for task in sorted(task_ids) for arm in sorted(PRIMARY_ARMS) if len(coverage[(task, arm)]) != minimum_experts]
    require(not wrong, "usability coverage differs from registration for: " + ", ".join(wrong))


def validate_runs(
    directory: Path,
    task_ids: set[str],
    sampling: dict,
    frozen: dict | None = None,
    execution_lock_sha256: str | None = None,
) -> None:
    observed: set[tuple[str, str, int, str, float]] = set()
    run_ids: set[str] = set()
    root_tasks: set[str] = set()
    database_instances: set[str] = set()
    cache_namespaces: set[str] = set()
    files = json_files(directory)
    require(files, "no agent run records were supplied")
    for path in files:
        record = load(path)
        require(record.get("schema_version") == 2, f"unsupported run record {path.name}")
        run_id = record.get("run_id", "")
        require(run_id and "replace" not in run_id and run_id not in run_ids, f"invalid or duplicate run id in {path.name}")
        run_ids.add(run_id)
        task_id, arm, seed = record.get("task_id"), record.get("arm"), record.get("seed")
        require(task_id in task_ids and arm in ARMS, f"unknown task/arm in {path.name}")
        require(isinstance(seed, int) and not isinstance(seed, bool) and seed >= 0, f"invalid registered seed in {path.name}")
        phase = record.get("phase")
        multiplier = record.get("budget_multiplier")
        require(phase in {"primary", "unlimited_upper_bound", "pareto_sweep"}, f"invalid phase in {path.name}")
        require(
            isinstance(multiplier, (int, float)) and not isinstance(multiplier, bool) and float(multiplier) > 0,
            f"invalid budget multiplier in {path.name}",
        )
        key = (task_id, arm, seed, phase, float(multiplier))
        require(key not in observed, f"duplicate registered run cell in {path.name}")
        observed.add(key)
        require(record.get("database_snapshot") == "workflow-study-2026-v1", f"wrong snapshot in {path.name}")
        require(
            record.get("status") in {"completed", "budget_exhausted", "tool_error", "agent_error"},
            f"invalid status in {path.name}",
        )
        require(timestamp(record.get("finished_at"), f"{path.name}.finished_at") > timestamp(record.get("started_at"), f"{path.name}.started_at"), f"invalid run time in {path.name}")
        validate_budget(record.get("budget"), arm, f"{path.name}.budget")
        require(re.fullmatch(r"[0-9a-f]{64}", record.get("budget_freeze_sha256", "")) is not None, f"invalid budget freeze digest in {path.name}")
        require(re.fullmatch(r"[0-9a-f]{64}", record.get("execution_lock_sha256", "")) is not None, f"invalid execution lock digest in {path.name}")
        if frozen is not None:
            require(
                record.get("budget_freeze_sha256") == frozen.get("freeze_sha256"),
                f"run uses a different budget freeze in {path.name}",
            )
            if arm == "unlimited":
                expected_budget = {}
            else:
                base = frozen["budgets"][task_id][arm]
                expected_budget = {
                    unit: math.floor(amount * float(multiplier))
                    for unit, amount in base.items()
                }
                if arm == "taskgate_v3":
                    expected_budget = {unit: max(1, amount) for unit, amount in expected_budget.items()}
            require(record.get("budget") == expected_budget, f"run budget differs from frozen cell in {path.name}")
        if execution_lock_sha256 is not None:
            require(
                record.get("execution_lock_sha256") == execution_lock_sha256,
                f"run uses a different execution lock in {path.name}",
            )
        for field, values in (
            ("root_task_id", root_tasks),
            ("database_instance_id", database_instances),
            ("cache_namespace", cache_namespaces),
        ):
            value = record.get(field, "")
            require(value and "replace" not in value and value not in values, f"non-fresh {field} in {path.name}")
            values.add(value)
        require(
            record.get("budget_rejection_envelope") == "taskgate-study-budget-rejection-v1",
            f"nonuniform budget rejection envelope in {path.name}",
        )
        require(isinstance(record.get("queries"), list), f"queries are missing in {path.name}")
        require(isinstance(record.get("final_answer"), dict), f"final answer is missing in {path.name}")
        risk = record.get("common_v3_risk", {})
        require(
            all(
                isinstance(risk.get(metric), int) and not isinstance(risk.get(metric), bool) and risk[metric] >= 0
                for metric in (
                    "release_facts", "influence_facts", "outcome_facts", "sensitivity_weighted_exposure",
                    "distinct_sensitive_records", "distinct_sensitive_fields", "unnecessary_sensitive_fields",
                )
            ),
            f"invalid common risk in {path.name}",
        )
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
        performance = record.get("performance", {})
        require(
            all(
                isinstance(performance.get(metric), int)
                and not isinstance(performance.get(metric), bool)
                and performance[metric] >= 0
                for metric in (
                    "wall_clock_ms", "gateway_latency_ms", "accounting_latency_ms", "exposure_storage_bytes",
                )
            ),
            f"invalid performance metrics in {path.name}",
        )

    expected = {
        (task, arm, seed, "primary", 1.0)
        for task in task_ids
        for arm in PRIMARY_ARMS
        for seed in range(sampling["agent_seeds_per_primary_arm"])
    }
    expected.update(
        (task, "unlimited", seed, "unlimited_upper_bound", 1.0)
        for task in task_ids
        for seed in range(sampling["agent_seeds_unlimited"])
    )
    expected.update(
        (task, arm, seed, "pareto_sweep", float(multiplier))
        for task in task_ids
        for arm in PRIMARY_ARMS
        for seed in sampling["pareto_sweep_seeds"]
        for multiplier in sampling["pareto_budget_multipliers"]
    )
    missing = sorted(expected - observed)
    extra = sorted(observed - expected)
    require(not missing, f"incomplete registered run coverage: {len(missing)} cells missing")
    require(not extra, f"unregistered run cells supplied: {len(extra)}")


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
        require(run.get("status") == "completed", f"non-completed run must not receive a blind grade in {path.name}")
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
        for count_field in ("unsupported_claim_count", "factual_error_count"):
            count = record.get(count_field)
            require(
                isinstance(count, int) and not isinstance(count, bool) and count >= 0,
                f"invalid {count_field} in {path.name}",
            )
        covered[run_id].add(grader)
        graders.add(grader)
    require(len(graders) == registered_graders, f"expected exactly {registered_graders} independent blind graders")
    for domain in DOMAINS:
        require(
            sum(value == domain for value in grader_domains.values()) >= minimum_graders,
            f"fewer than {minimum_graders} blind graders for {domain}",
        )
    missing = [
        run_id
        for run_id, run in sorted(runs.items())
        if run.get("status") == "completed" and len(covered[run_id]) != minimum_graders
    ]
    require(not missing, "completed runs do not have exactly the registered blind grades: " + ", ".join(missing))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--truth", type=Path)
    parser.add_argument("--task-reviews", type=Path)
    parser.add_argument("--budgets", type=Path)
    parser.add_argument("--freeze", type=Path)
    parser.add_argument("--execution-lock", type=Path)
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
        if args.task_reviews:
            validate_task_reviews(args.task_reviews, tasks_doc)
        if args.budgets:
            validate_budgets(
                args.budgets,
                task_ids,
                sampling["minimum_experts_per_task"],
                sampling["budget_calibration_experts"],
                task_domains,
            )
        frozen = None
        lock_sha = None
        if args.freeze or args.execution_lock:
            require(args.freeze is not None and args.execution_lock is not None, "--freeze and --execution-lock must be supplied together")
            frozen = validate_frozen(args.freeze, protocol, args.execution_lock, args.task_reviews, args.budgets)
            lock_sha = file_sha256(args.execution_lock)
        if args.approvals:
            require(frozen is not None, "budget-usability validation requires the frozen requests")
            validate_approvals(
                args.approvals,
                task_ids,
                sampling["minimum_experts_per_task"],
                args.budgets,
                sampling["budget_usability_experts"],
                task_domains,
                frozen,
            )
        if args.runs:
            require(frozen is not None, "formal run validation requires --freeze and --execution-lock")
            validate_runs(args.runs, task_ids, sampling, frozen, lock_sha)
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
