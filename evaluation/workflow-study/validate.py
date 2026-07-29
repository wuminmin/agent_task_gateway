#!/usr/bin/env python3
"""Validate the participant-free controlled Agent workflow benchmark."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import re
from collections import defaultdict
from pathlib import Path

import study_risk


HERE = Path(__file__).resolve().parent
TASKS = HERE / "tasks.json"
CALIBRATION_TASKS = HERE / "calibration-tasks.json"
PROTOCOL = HERE / "protocol.json"
DOMAINS = {"finance", "risk_compliance", "customer_operations"}
PRIMARY_ARMS = {"taskgate_v3", "query_count", "returned_rows", "serialized_bytes"}
ARMS = PRIMARY_ARMS | {"unlimited"}
BUDGET_LEVELS = (0.25, 0.5, 0.75, 1.0)
ZERO_SHA256 = "0" * 64
AUTOMATIC_METHODS = {
    "exact",
    "set_f1",
    "numeric_absolute_0_01",
    "numeric_absolute_0_1",
    "numeric_relative_2pct",
    "ordered_list_overlap",
    "exact_literal",
    "trace_forbidden_columns",
    "trace_allowed_columns",
    "trace_query_bound",
    "query_trace_rule",
}
BUDGET_FIELDS = {
    "taskgate_v3": {"release_facts", "influence_facts", "outcome_facts"},
    "query_count": {"successful_queries"},
    "returned_rows": {"returned_rows"},
    "serialized_bytes": {"serialized_bytes"},
    "unlimited": set(),
}
BUDGET_MAX = {
    "taskgate_v3": {"release_facts": 1000000, "influence_facts": 10000000, "outcome_facts": 1000},
    "query_count": {"successful_queries": 100},
    "returned_rows": {"returned_rows": 100000},
    "serialized_bytes": {"serialized_bytes": 100000000},
    "unlimited": {},
}
GATEWAY_AUDIT_NATIVE_LIMITS = {"queries": 100, "rows": 100000, "db_ms": 120000}
GATEWAY_AUDIT_EXPOSURE_LIMITS = {
    "release_facts": 1000000,
    "influence_facts": 10000000,
    "outcome_facts": 1000,
}
USAGE_UNIT_MAPPING = {
    "taskgate_v3": {
        "release_facts": "common_v3_risk.release_facts",
        "influence_facts": "common_v3_risk.influence_facts",
        "outcome_facts": "common_v3_risk.outcome_facts",
    },
    "query_count": {"successful_queries": "native_usage.successful_queries"},
    "returned_rows": {"returned_rows": "native_usage.returned_rows"},
    "serialized_bytes": {"serialized_bytes": "native_usage.serialized_bytes"},
}
FROZEN_SOURCE_PATHS = (
    "validate.py",
    "controller.py",
    "deepseek_agent_adapter.py",
    "lock_execution.py",
    "run_study.py",
    "freeze_budgets.py",
    "analyze.py",
    "certify_collection.py",
    "study_risk.py",
    "test_design.py",
    "test_deepseek_adapter.py",
    "test_certify_collection.py",
    "export-ground-truth.sh",
    "system-prompt.txt",
    "agent-tool-surface.json",
    "tasks.json",
    "calibration-tasks.json",
    "protocol.json",
    "risk-preference-card.json",
    "unit-cards.json",
    "compose.yaml",
    "db/05-workflow-study.sql",
    "db/10-ground-truth.sql",
    "db/15-workflow-reader.sql",
    "raw/ground-truth.json",
    "sensitivity-map.json",
    "essential-columns.json",
    "catalog.yaml",
    "../../compose.yaml",
    "../../Dockerfile",
    "../../go.mod",
    "../../go.sum",
    "../../.dockerignore",
    "../../db/init/00-schema.sql",
    "../../db/init/10-reader.sh",
)
FROZEN_SOURCE_TREE_PATHS = (
    "../../internal",
    "../../cmd/gateway",
    "../../db/control-init",
)
COMMON_RISK_FIELDS = (
    "release_facts",
    "influence_facts",
    "outcome_facts",
    "sensitivity_weighted_exposure",
    "distinct_sensitive_records",
    "distinct_sensitive_fields",
    "unnecessary_sensitive_fields",
)
NEUTRAL_DISCLOSURE_FIELDS = (
    "released_sensitive_records",
    "released_sensitive_fields",
    "released_sensitive_cells",
    "released_sensitive_values",
    "disclosed_outcome_propositions",
    "disclosed_negative_propositions",
)
NATIVE_USAGE_FIELDS = ("successful_queries", "returned_rows", "serialized_bytes")
PERFORMANCE_FIELDS = ("wall_clock_ms", "gateway_latency_ms", "accounting_latency_ms", "exposure_storage_bytes")
PROVIDER_TOKEN_USAGE_FIELDS = (
    "prompt_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens",
    "completion_tokens", "reasoning_tokens", "total_tokens",
)


def load(path: Path) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ValueError(f"cannot read JSON {path}: {error}") from error


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def canonical_sha256(value: object) -> str:
    encoded = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()
    return hashlib.sha256(encoded).hexdigest()


def canonical_json_bytes(value: object) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False,
    ).encode("utf-8")


def registered_run_id(
    study_id: str,
    schedule_kind: str,
    task_id: str,
    arm: str,
    replicate: int,
    level: float,
) -> str:
    """Return the source-registered identity for one schedule cell."""
    material = f"{study_id}\0{schedule_kind}\0{task_id}\0{arm}\0{replicate}\0{level:.2f}".encode()
    return "ws-" + hashlib.sha256(material).hexdigest()[:24]


def file_sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def timestamp(value: object, label: str) -> dt.datetime:
    require(isinstance(value, str) and value.strip(), f"{label} must be an RFC3339 timestamp")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise ValueError(f"{label} is not an RFC3339 timestamp") from error
    require(parsed.tzinfo is not None, f"{label} must include a timezone")
    return parsed


def json_files(directory: Path) -> list[Path]:
    require(directory.is_dir(), f"collection directory is missing: {directory}")
    return sorted(path for path in directory.glob("*.json") if ".example." not in path.name)


def record_digests(directory: Path) -> list[dict[str, str]]:
    return [{"name": path.name, "sha256": file_sha256(path)} for path in json_files(directory)]


def source_file_digests(*, include_generated_truth: bool = True) -> dict[str, str]:
    result = {}
    for relative in FROZEN_SOURCE_PATHS:
        if relative == "raw/ground-truth.json" and not include_generated_truth:
            continue
        path = HERE / relative
        require(path.is_file(), f"frozen source is missing: {relative}")
        result[relative] = file_sha256(path)
    for relative in FROZEN_SOURCE_TREE_PATHS:
        root = (HERE / relative).resolve()
        require(root.is_dir(), f"frozen source tree is missing: {relative}")
        digest = hashlib.sha256()
        files = sorted(path for path in root.rglob("*") if path.is_file())
        require(bool(files), f"frozen source tree is empty: {relative}")
        for path in files:
            digest.update(path.relative_to(root).as_posix().encode())
            digest.update(b"\0")
            digest.update(path.read_bytes())
            digest.update(b"\0")
        result[f"tree:{relative}"] = digest.hexdigest()
    return result


def level_key(level: float) -> str:
    return f"{float(level):.2f}"


def sampling_plan(protocol: dict) -> dict:
    sampling = protocol.get("sampling", {})
    expected = {
        "calibration_tasks": 6,
        "evaluation_tasks": 12,
        "calibration_replicates": 3,
        "calibration_runs": 18,
        "budget_levels": list(BUDGET_LEVELS),
        "evaluation_replicates_per_level": 3,
        "budgeted_evaluation_runs": 576,
        "unlimited_replicates": 5,
        "unlimited_runs": 60,
        "planned_evaluation_runs": 636,
        "total_agent_runs": 654,
    }
    for field, value in expected.items():
        require(sampling.get(field) == value, f"sampling.{field} differs from the registered design")
    return expected


def _manifest_identity(document: dict) -> str:
    return str(document.get("task_set_id") or document.get("calibration_set_id") or document.get("study_id") or "")


def _validate_task_manifest(document: dict, *, count: int, per_domain: int, label: str) -> set[str]:
    require(document.get("schema_version") in {1, 2}, f"unsupported {label} manifest")
    require(_manifest_identity(document), f"{label} manifest has no identity")
    tasks = document.get("tasks", [])
    require(len(tasks) == count, f"the registered design requires exactly {count} {label} tasks")
    document_source = " ".join(
        str(document.get(field, "")) for field in ("source_method", "purpose", "provenance")
    ).lower()
    seen: set[str] = set()
    domains: dict[str, int] = defaultdict(int)
    for task in tasks:
        task_id = task.get("id", "")
        require(re.fullmatch(r"[A-Z][A-Z0-9-]{2,31}", task_id) is not None, f"invalid {label} task id {task_id!r}")
        require(task_id not in seen, f"duplicate {label} task {task_id}")
        seen.add(task_id)
        domain = task.get("domain")
        require(domain in DOMAINS, f"invalid domain for {task_id}")
        domains[domain] += 1
        require(task.get("difficulty") in {"low", "medium", "high"}, f"task {task_id} lacks registered difficulty")
        source = str(task.get("case_source") or document_source).lower()
        require("synthetic" in source, f"task {task_id} is not identified as synthetic")
        require(len(task.get("prompt", "").split()) >= 20, f"task {task_id} prompt is not a workflow request")
        products = task.get("products")
        columns = task.get("approved_columns")
        require(
            isinstance(products, list) and products and len(products) == len(set(products))
            and all(isinstance(product, str) and product for product in products),
            f"task {task_id} has invalid products",
        )
        require(isinstance(columns, dict) and set(columns) == set(products), f"task {task_id} columns do not match products")
        require(
            all(isinstance(values, list) and values and len(values) == len(set(values)) for values in columns.values()),
            f"task {task_id} has invalid approved columns",
        )
        scope = task.get("scope")
        require(isinstance(scope, dict) and scope, f"task {task_id} lacks a registered scope")
        if label == "calibration":
            required = task.get("required_answer_fields")
            require(
                isinstance(required, list) and required and len(required) == len(set(required))
                and all(isinstance(field, str) and field for field in required),
                f"calibration task {task_id} has invalid required_answer_fields",
            )
            continue
        require(task.get("ground_truth_key") == task_id, f"task {task_id} ground-truth key differs")
        evidence_columns = task.get("evidence_required_columns")
        approved_union = {field for values in columns.values() for field in values}
        require(
            isinstance(evidence_columns, list) and evidence_columns
            and len(evidence_columns) == len(set(evidence_columns))
            and all(isinstance(field, str) and field in approved_union for field in evidence_columns),
            f"task {task_id} has invalid evidence-required columns",
        )
        rubric = task.get("rubric", [])
        require(4 <= len(rubric) <= 8, f"task {task_id} must have four to eight rubric goals")
        weights = [item.get("weight") for item in rubric]
        require(all(isinstance(weight, int) and weight > 0 for weight in weights), f"task {task_id} has invalid weights")
        require(sum(weights) == 100, f"task {task_id} rubric weights do not sum to 100")
        require(any(item.get("critical") is True for item in rubric), f"task {task_id} has no critical goal")
        require(
            any(item.get("critical") is True and item.get("answer_path") for item in rubric),
            f"task {task_id} has no critical answer goal",
        )
        item_ids = [item.get("id") for item in rubric]
        require(len(item_ids) == len(set(item_ids)) and all(item_ids), f"task {task_id} repeats a rubric item")
        for item in rubric:
            method = item.get("method")
            require(method in AUTOMATIC_METHODS, f"task {task_id} uses non-automatic scoring method {method!r}")
            if method in {
                "exact", "set_f1", "numeric_absolute_0_01", "numeric_absolute_0_1",
                "numeric_relative_2pct", "ordered_list_overlap",
            }:
                require(item.get("answer_path"), f"task {task_id} automated answer item lacks answer_path")
            if method == "exact_literal":
                require(item.get("answer_path"), f"task {task_id} exact_literal item lacks answer_path")
                require("expected" in item, f"task {task_id} exact_literal item lacks expected")
            if method == "trace_query_bound":
                minimum = item.get("min_query_attempts")
                maximum = item.get("max_query_attempts")
                require(
                    isinstance(minimum, int) and not isinstance(minimum, bool) and minimum >= 0
                    and isinstance(maximum, int) and not isinstance(maximum, bool) and maximum >= minimum,
                    f"task {task_id} trace attempt bounds are invalid",
                )
                admitted_minimum = item.get("min_admitted_queries", 0)
                admitted_maximum = item.get("max_admitted_queries", maximum)
                require(
                    isinstance(admitted_minimum, int) and not isinstance(admitted_minimum, bool)
                    and admitted_minimum >= 0
                    and isinstance(admitted_maximum, int) and not isinstance(admitted_maximum, bool)
                    and admitted_maximum >= admitted_minimum and admitted_maximum <= maximum,
                    f"task {task_id} admitted-query bounds are invalid",
                )
    require(dict(domains) == {domain: per_domain for domain in DOMAINS}, f"{label} domain balance differs")
    return seen


def validate_design() -> tuple[dict, dict, dict]:
    tasks_doc = load(TASKS)
    calibration_doc = load(CALIBRATION_TASKS)
    protocol = load(PROTOCOL)
    require(protocol.get("schema_version") == 3, "unsupported study protocol")
    require(protocol.get("status") == "designed_not_collected", "protocol must not claim uncollected outcomes")
    plan = sampling_plan(protocol)
    evaluation_ids = _validate_task_manifest(
        tasks_doc, count=plan["evaluation_tasks"], per_domain=4, label="evaluation",
    )
    calibration_ids = _validate_task_manifest(
        calibration_doc, count=plan["calibration_tasks"], per_domain=2, label="calibration",
    )
    require(evaluation_ids.isdisjoint(calibration_ids), "calibration and evaluation task IDs overlap")
    protocol_arms = {arm.get("id") for arm in protocol.get("arms", [])}
    require(protocol_arms == ARMS, "registered policy arms differ")
    require((HERE / "controller.py").is_file(), "missing baseline buffer-before-release controller")
    require((HERE / "system-prompt.txt").is_file(), "missing frozen Agent system prompt")
    require((HERE / "agent-tool-surface.json").is_file(), "missing frozen Agent tool surface")
    for template in ("agent-run.example.json", "algorithmic-budget-freeze.example.json", "execution-lock.example.json"):
        require((HERE / "templates" / template).is_file(), f"missing benchmark template {template}")
    return tasks_doc, calibration_doc, protocol


def load_tasks() -> tuple[dict[str, dict], dict[str, dict]]:
    evaluation, calibration, _ = validate_design()
    return (
        {task["id"]: task for task in evaluation["tasks"]},
        {task["id"]: task for task in calibration["tasks"]},
    )


def has_path(value: object, path: str) -> bool:
    current = value
    for part in path.split("."):
        if not isinstance(current, dict) or part not in current:
            return False
        current = current[part]
    return True


def validate_truth(path: Path, task_ids: set[str], tasks_doc: dict | None = None) -> None:
    truth = load(path)
    require(set(truth) == task_ids, "exported ground truth does not cover exactly the evaluation tasks")
    for task_id, answer in truth.items():
        require(isinstance(answer, dict) and answer, f"ground truth {task_id} is empty")
    if tasks_doc is not None:
        for task in tasks_doc["tasks"]:
            for item in task["rubric"]:
                if item["method"] in {
                    "exact", "set_f1", "numeric_absolute_0_01", "numeric_absolute_0_1",
                    "numeric_relative_2pct", "ordered_list_overlap",
                }:
                    require(has_path(truth[task["id"]], item["answer_path"]), f"ground truth {task['id']} lacks {item['answer_path']}")


def validate_execution_lock(path: Path, study_id: str) -> dict:
    lock = load(path)
    require(lock.get("schema_version") == 2 and lock.get("study_id") == study_id, "invalid execution lock identity")
    for field in ("provider", "model", "model_version"):
        require(isinstance(lock.get(field), str) and lock[field] and "replace" not in lock[field], f"invalid execution lock {field}")
    require(
        lock.get("thinking_mode") == "disabled",
        "execution lock must explicitly freeze DeepSeek non-thinking mode",
    )
    expected_versions = {
        "deepseek-v4-flash": "DeepSeek-V4-Flash",
        "deepseek-v4-pro": "DeepSeek-V4-Pro",
    }
    require(
        lock.get("model") in expected_versions
        and lock.get("model_version") == expected_versions[lock["model"]],
        "execution lock model alias/release label is not a registered DeepSeek V4 pair",
    )
    require(
        isinstance(lock.get("campaign_id"), str)
        and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", lock["campaign_id"]) is not None
        and "replace" not in lock["campaign_id"],
        "invalid execution lock campaign_id",
    )
    timestamp(lock.get("locked_at"), "execution lock locked_at")
    require(
        isinstance(lock.get("temperature"), (int, float)) and not isinstance(lock.get("temperature"), bool)
        and math.isfinite(float(lock["temperature"])) and 0 <= float(lock["temperature"]) <= 2,
        "invalid execution lock temperature",
    )
    require(
        isinstance(lock.get("top_p"), (int, float)) and not isinstance(lock.get("top_p"), bool)
        and math.isfinite(float(lock["top_p"])) and 0 < float(lock["top_p"]) <= 1,
        "invalid execution lock top_p",
    )
    require(
        isinstance(lock.get("max_tokens"), int) and not isinstance(lock.get("max_tokens"), bool)
        and 1 <= lock["max_tokens"] <= 8192,
        "invalid execution lock max_tokens",
    )
    require(lock.get("api_base_url") == "https://api.deepseek.com", "execution lock must use the official HTTPS DeepSeek endpoint")
    require(
        isinstance(lock.get("request_timeout_seconds"), int)
        and not isinstance(lock["request_timeout_seconds"], bool)
        and 1 <= lock["request_timeout_seconds"] <= 1800,
        "invalid execution lock request_timeout_seconds",
    )
    require(
        isinstance(lock.get("adapter_timeout_seconds"), int)
        and not isinstance(lock["adapter_timeout_seconds"], bool)
        and lock["request_timeout_seconds"] <= lock["adapter_timeout_seconds"] <= 86400,
        "invalid execution lock adapter_timeout_seconds",
    )
    require(
        isinstance(lock.get("max_tool_turns"), int)
        and not isinstance(lock["max_tool_turns"], bool)
        and 1 <= lock["max_tool_turns"] <= 64,
        "invalid execution lock max_tool_turns",
    )
    retry = lock.get("api_retry")
    require(isinstance(retry, dict), "execution lock lacks API retry policy")
    require(
        set(retry) == {
            "max_attempts", "initial_backoff_seconds", "max_backoff_seconds",
            "retryable_http_statuses", "retry_insufficient_system_resource",
        }
        and isinstance(retry["max_attempts"], int) and not isinstance(retry["max_attempts"], bool)
        and 1 <= retry["max_attempts"] <= 10
        and isinstance(retry["initial_backoff_seconds"], (int, float))
        and isinstance(retry["max_backoff_seconds"], (int, float))
        and 0 < retry["initial_backoff_seconds"] <= retry["max_backoff_seconds"] <= 300
        and retry["retryable_http_statuses"] == [429, 500, 502, 503, 504]
        and retry["retry_insufficient_system_resource"] is True,
        "invalid execution lock API retry policy",
    )
    infrastructure_retry = lock.get("infrastructure_retry")
    require(
        isinstance(infrastructure_retry, dict)
        and set(infrastructure_retry) == {
            "compose_start_max_attempts", "compose_start_backoff_seconds",
        }
        and isinstance(infrastructure_retry["compose_start_max_attempts"], int)
        and not isinstance(infrastructure_retry["compose_start_max_attempts"], bool)
        and 1 <= infrastructure_retry["compose_start_max_attempts"] <= 5
        and isinstance(infrastructure_retry["compose_start_backoff_seconds"], (int, float))
        and not isinstance(infrastructure_retry["compose_start_backoff_seconds"], bool)
        and math.isfinite(float(infrastructure_retry["compose_start_backoff_seconds"]))
        and 0 <= infrastructure_retry["compose_start_backoff_seconds"] <= 30,
        "invalid execution lock infrastructure retry policy",
    )
    pricing = lock.get("pricing_usd_per_million_tokens")
    expected_pricing = {
        "deepseek-v4-flash": {"prompt_cache_hit": 0.0028, "prompt_cache_miss": 0.14, "completion": 0.28},
        "deepseek-v4-pro": {"prompt_cache_hit": 0.003625, "prompt_cache_miss": 0.435, "completion": 0.87},
    }
    require(
        isinstance(pricing, dict)
        and set(pricing) == {"prompt_cache_hit", "prompt_cache_miss", "completion"}
        and all(
            isinstance(value, (int, float)) and not isinstance(value, bool)
            and math.isfinite(float(value)) and value > 0
            for value in pricing.values()
        )
        and pricing == expected_pricing[lock["model"]]
        and lock.get("pricing_source") == "https://api-docs.deepseek.com/quick_start/pricing/",
        "invalid execution lock pricing snapshot",
    )
    limits = lock.get("phase_cost_limits_usd")
    require(
        isinstance(limits, dict) and set(limits) == {"calibration", "evaluation"}
        and all(
            isinstance(value, (int, float)) and not isinstance(value, bool)
            and math.isfinite(float(value)) and value > 0
            for value in limits.values()
        )
        and limits["calibration"] < limits["evaluation"] <= 100,
        "invalid execution lock phase cost limits",
    )
    images = lock.get("container_images")
    require(
        isinstance(images, dict) and set(images) == {"gateway", "oa_demo", "postgres"},
        "execution lock lacks the exact container image set",
    )
    for name, image in images.items():
        require(
            isinstance(image, dict)
            and set(image) == {"requested_reference", "image_id", "repo_digests"}
            and isinstance(image["requested_reference"], str)
            and bool(image["requested_reference"])
            and not any(character.isspace() for character in image["requested_reference"])
            and re.fullmatch(r"sha256:[0-9a-f]{64}", image["image_id"] or "") is not None
            and isinstance(image["repo_digests"], list)
            and image["repo_digests"] == sorted(set(image["repo_digests"]))
            and all(
                isinstance(digest, str)
                and re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", digest) is not None
                for digest in image["repo_digests"]
            ),
            f"invalid execution lock container image {name}",
        )
    runtime = lock.get("container_runtime")
    require(
        isinstance(runtime, dict)
        and set(runtime) == {"docker_server_version", "docker_compose_version"}
        and all(
            isinstance(value, str) and value and not any(character.isspace() for character in value)
            for value in runtime.values()
        ),
        "invalid execution lock container runtime",
    )
    for field in ("system_prompt_sha256", "tool_surface_sha256", "agent_adapter_sha256", "answer_schema_sha256"):
        require(re.fullmatch(r"[0-9a-f]{64}", lock.get(field, "")) is not None, f"invalid execution lock {field}")
    evaluation = load(TASKS)
    calibration = load(CALIBRATION_TASKS)
    answer_contract = {
        task["id"]: sorted({item["answer_path"].split(".")[0] for item in task["rubric"] if item.get("answer_path")})
        for task in evaluation["tasks"]
    }
    answer_contract.update(
        {task["id"]: sorted(task["required_answer_fields"]) for task in calibration["tasks"]}
    )
    expected = {
        "system_prompt_sha256": file_sha256(HERE / "system-prompt.txt"),
        "tool_surface_sha256": file_sha256(HERE / "agent-tool-surface.json"),
        "agent_adapter_sha256": file_sha256(HERE / "deepseek_agent_adapter.py"),
        "answer_schema_sha256": canonical_sha256(answer_contract),
    }
    for field, digest in expected.items():
        require(lock[field] == digest, f"execution lock {field} does not match source")
    return lock


def validate_budget(value: object, arm: str, label: str, *, allow_zero: bool = False) -> None:
    require(isinstance(value, dict), f"{label} must be an object")
    require(set(value) == BUDGET_FIELDS[arm], f"{label} has the wrong units for {arm}")
    minimum = 0 if allow_zero else 1
    require(
        all(isinstance(amount, int) and not isinstance(amount, bool) and amount >= minimum for amount in value.values()),
        f"{label} values must be integers >= {minimum}",
    )
    require(all(amount <= BUDGET_MAX[arm][unit] for unit, amount in value.items()), f"{label} exceeds the operational range")


def validate_algorithmic_freeze(
    path: Path,
    protocol: dict,
    execution_lock: Path,
    calibration_runs: Path,
) -> dict:
    frozen = load(path)
    claimed = frozen.get("freeze_sha256", "")
    payload = dict(frozen)
    payload.pop("freeze_sha256", None)
    require(claimed == canonical_sha256(payload), "algorithmic budget freeze digest mismatch")
    require(
        frozen.get("schema_version") == 2
        and frozen.get("status") == "frozen_from_held_out_calibration"
        and frozen.get("study_id") == protocol["study_id"],
        "invalid algorithmic budget freeze identity",
    )
    frozen_at = timestamp(frozen.get("frozen_at"), "frozen_at")
    require(frozen.get("evaluation_task_manifest_sha256") == file_sha256(TASKS), "evaluation tasks differ from freeze")
    require(frozen.get("calibration_task_manifest_sha256") == file_sha256(CALIBRATION_TASKS), "calibration tasks differ from freeze")
    require(frozen.get("protocol_sha256") == file_sha256(PROTOCOL), "protocol differs from freeze")
    lock = validate_execution_lock(execution_lock, protocol["study_id"])
    require(frozen.get("execution_lock_sha256") == file_sha256(execution_lock), "execution lock differs from freeze")
    require(frozen.get("source_file_sha256") == source_file_digests(), "database/oracle/risk sources differ from freeze")
    require(frozen.get("usage_unit_mapping") == USAGE_UNIT_MAPPING, "usage-unit mapping differs from registration")
    require(frozen.get("levels") == list(BUDGET_LEVELS), "freeze budget levels differ from registration")
    require(
        frozen.get("level_rule") == "max(1, floor(domain_lower_median_usage * level)) component-wise",
        "freeze level rule differs from registration",
    )
    calibration_records = frozen.get("calibration_run_records")
    require(
        isinstance(calibration_records, list)
        and len(calibration_records) == sampling_plan(protocol)["calibration_runs"]
        and len({item.get("name") for item in calibration_records if isinstance(item, dict)}) == len(calibration_records)
        and all(
            isinstance(item, dict)
            and isinstance(item.get("name"), str) and item["name"].endswith(".json")
            and re.fullmatch(r"[0-9a-f]{64}", item.get("sha256", "")) is not None
            for item in calibration_records
        ),
        "freeze does not bind exactly 18 unique calibration run records",
    )
    require(frozen.get("calibration_run_records") == record_digests(calibration_runs), "calibration runs differ from freeze")
    calibration_doc = load(CALIBRATION_TASKS)
    observed_calibration = validate_calibration_runs(
        calibration_runs,
        calibration_doc,
        protocol,
        file_sha256(execution_lock),
        lock,
    )
    for record in observed_calibration:
        require(
            frozen_at > timestamp(record["finished_at"], f"{record['run_id']}.finished_at"),
            "algorithmic freeze must be later than every calibration completion",
        )
    domains = frozen.get("domains")
    require(isinstance(domains, dict) and set(domains) == DOMAINS, "freeze does not cover exactly three domains")
    for domain, detail in domains.items():
        require(detail.get("completed_calibration_runs") == 6, f"{domain} freeze must aggregate six traces")
        base = detail.get("base")
        levels = detail.get("levels")
        require(isinstance(base, dict) and set(base) == PRIMARY_ARMS, f"{domain} base usage is incomplete")
        require(isinstance(levels, dict) and set(levels) == {level_key(x) for x in BUDGET_LEVELS}, f"{domain} levels differ")
        for arm in PRIMARY_ARMS:
            validate_budget(base[arm], arm, f"{domain}.base.{arm}", allow_zero=True)
        task_domains = {task["id"]: task["domain"] for task in load(CALIBRATION_TASKS)["tasks"]}
        traces = [record for record in observed_calibration if task_domains[record["task_id"]] == domain]
        expected_base = {}
        for arm in PRIMARY_ARMS:
            expected_base[arm] = {}
            for unit, path_label in USAGE_UNIT_MAPPING[arm].items():
                values = []
                for record in traces:
                    current: object = record
                    for component in path_label.split("."):
                        require(isinstance(current, dict) and component in current, f"calibration usage lacks {path_label}")
                        current = current[component]
                    require(isinstance(current, int) and not isinstance(current, bool) and current >= 0, f"invalid usage at {path_label}")
                    values.append(current)
                ordered = sorted(values)
                expected_base[arm][unit] = ordered[(len(ordered) - 1) // 2]
        require(base == expected_base, f"{domain} base is not the component-wise lower median of calibration traces")
        for level in BUDGET_LEVELS:
            cell = levels[level_key(level)]
            require(isinstance(cell, dict) and set(cell) == PRIMARY_ARMS, f"{domain}/{level} arms differ")
            for arm in PRIMARY_ARMS:
                expected = {unit: max(1, math.floor(amount * level)) for unit, amount in base[arm].items()}
                require(cell[arm] == expected, f"{domain}/{arm}/{level} is not the registered floor-and-clamp budget")
                validate_budget(cell[arm], arm, f"{domain}.{level}.{arm}")
    return frozen


def _nonnegative_int_fields(value: object, fields: tuple[str, ...], label: str) -> None:
    require(isinstance(value, dict), f"{label} must be an object")
    require(set(value) >= set(fields), f"{label} is missing registered fields")
    require(
        all(isinstance(value[field], int) and not isinstance(value[field], bool) and value[field] >= 0 for field in fields),
        f"{label} has invalid nonnegative counts",
    )


def _validate_admitted_responses(record: dict, label: str) -> tuple[dict[str, int], set[str], int]:
    queries = record["queries"]
    require(all(isinstance(entry, dict) for entry in queries), f"{label}.queries contains a non-object")
    request_ids: set[str] = set()
    admitted_query_ids: set[str] = set()
    usage = {"successful_queries": 0, "returned_rows": 0, "serialized_bytes": 0}
    rejection_count = 0
    for index, entry in enumerate(queries):
        query_label = f"{label}.queries[{index}]"
        request_id = entry.get("request_id")
        require(isinstance(request_id, str) and request_id, f"{query_label} lacks request_id")
        require(request_id not in request_ids, f"{label} repeats a query request_id")
        request_ids.add(request_id)
        require(isinstance(entry.get("admitted"), bool), f"{query_label}.admitted is invalid")
        if "budget_rejected" in entry:
            require(isinstance(entry["budget_rejected"], bool), f"{query_label}.budget_rejected is invalid")
        if entry.get("budget_rejected") is True:
            require(not entry["admitted"], f"{query_label} cannot be admitted and budget-rejected")
            rejection_count += 1
        if not entry["admitted"]:
            require(
                "admitted_response_canonical" not in entry
                and "admitted_response_sha256" not in entry
                and "query_id" not in entry,
                f"{query_label} exposes evidence for a response not admitted to the Agent",
            )
            continue
        query_id = entry.get("query_id")
        require(isinstance(query_id, str) and query_id, f"{query_label} lacks query_id")
        require(query_id not in admitted_query_ids, f"{label} repeats an admitted query_id")
        admitted_query_ids.add(query_id)
        canonical = entry.get("admitted_response_canonical")
        digest = entry.get("admitted_response_sha256")
        require(isinstance(canonical, str), f"{query_label} lacks canonical admitted response")
        payload = canonical.encode("utf-8")
        require(hashlib.sha256(payload).hexdigest() == digest, f"{query_label} admitted response hash mismatch")
        try:
            visible = json.loads(canonical)
        except json.JSONDecodeError as error:
            raise ValueError(f"{query_label} admitted response is not JSON") from error
        require(
            isinstance(visible, dict) and set(visible) == {"columns", "rows", "row_count", "limited"},
            f"{query_label} admitted response shape differs",
        )
        require(canonical_json_bytes(visible) == payload, f"{query_label} admitted response is not canonical JSON")
        require(isinstance(visible["columns"], list), f"{query_label} columns are invalid")
        require(isinstance(visible["rows"], list), f"{query_label} rows are invalid")
        row_count = visible["row_count"]
        require(
            isinstance(row_count, int) and not isinstance(row_count, bool) and row_count >= 0,
            f"{query_label} row_count is invalid",
        )
        require(row_count == len(visible["rows"]), f"{query_label} row_count differs from released rows")
        require(isinstance(visible["limited"], bool), f"{query_label} limited flag is invalid")
        require(entry.get("row_count") == row_count, f"{query_label} trace row_count differs")
        require(entry.get("serialized_bytes") == len(payload), f"{query_label} trace byte count differs")
        usage["successful_queries"] += 1
        usage["returned_rows"] += row_count
        usage["serialized_bytes"] += len(payload)
    return usage, admitted_query_ids, rejection_count


def _validate_fact_evidence(record: dict, task: dict, admitted_query_ids: set[str], label: str) -> None:
    facts = record.get("fact_evidence")
    require(isinstance(facts, list), f"{label}.fact_evidence must be a list")
    require(
        record.get("fact_evidence_sha256") == canonical_sha256(facts),
        f"{label}.fact_evidence hash mismatch",
    )
    seen: set[tuple[str, str]] = set()
    linked_queries: set[str] = set()
    order: list[tuple[str, str]] = []
    for index, fact in enumerate(facts):
        fact_label = f"{label}.fact_evidence[{index}]"
        require(isinstance(fact, dict), f"{fact_label} is not an object")
        ledger_kind = fact.get("ledger_kind")
        fact_sha = fact.get("fact_sha256")
        require(ledger_kind in {"RELEASE", "INFLUENCE", "OUTCOME"}, f"{fact_label} ledger kind is invalid")
        require(re.fullmatch(r"[0-9a-f]{64}", str(fact_sha)) is not None, f"{fact_label} fact hash is invalid")
        require(isinstance(fact.get("identity"), dict), f"{fact_label} identity is invalid")
        query_ids = fact.get("query_ids")
        require(
            isinstance(query_ids, list) and query_ids
            and query_ids == sorted(set(query_ids))
            and all(isinstance(query_id, str) and query_id in admitted_query_ids for query_id in query_ids),
            f"{fact_label} query links are invalid",
        )
        key = (ledger_kind, fact_sha)
        require(key not in seen, f"{label} repeats a Fact evidence item")
        seen.add(key)
        order.append(key)
        linked_queries.update(query_ids)
    require(order == sorted(order), f"{label}.fact_evidence is not in canonical order")
    require(linked_queries == admitted_query_ids, f"{label}.fact_evidence does not cover exactly the admitted queries")
    recomputed = study_risk.measure(facts, task)
    expected_common = {field: recomputed[field] for field in COMMON_RISK_FIELDS}
    expected_neutral = {field: recomputed[field] for field in NEUTRAL_DISCLOSURE_FIELDS}
    require(record.get("common_v3_risk") == expected_common, f"{label}.common_v3_risk is not evidence-derived")
    require(record.get("neutral_disclosure") == expected_neutral, f"{label}.neutral_disclosure is not evidence-derived")


def _audit_nonnegative_map(value: object, fields: set[str], label: str) -> dict:
    require(isinstance(value, dict) and set(value) >= fields, f"{label} is incomplete")
    require(
        all(isinstance(value[field], int) and not isinstance(value[field], bool) and value[field] >= 0 for field in fields),
        f"{label} has invalid counts",
    )
    return value


def _validate_gateway_budget_audit(record: dict, label: str) -> None:
    audit = record.get("gateway_budget_audit")
    require(isinstance(audit, dict), f"{label}.gateway_budget_audit must be an object")
    require(
        record.get("gateway_budget_audit_sha256") == canonical_sha256(audit),
        f"{label}.gateway_budget_audit hash mismatch",
    )
    available = audit.get("available")
    require(isinstance(available, bool), f"{label}.gateway_budget_audit availability is invalid")
    if not available:
        require(
            record["status"] != "completed"
            and record["root_task_id"].startswith("not-created-")
            and record["queries"] == []
            and record["fact_evidence"] == [],
            f"{label} may omit a budget audit only when task creation failed before querying",
        )
        return
    snapshot = audit.get("snapshot")
    require(isinstance(snapshot, dict), f"{label}.gateway budget snapshot is missing")
    require(snapshot.get("task_id") == record["root_task_id"], f"{label}.gateway audit task differs")
    native = snapshot.get("budget")
    require(isinstance(native, dict), f"{label}.gateway native budget is missing")
    limits = _audit_nonnegative_map(native.get("limits"), {"queries", "rows", "db_ms"}, f"{label}.gateway limits")
    used = _audit_nonnegative_map(native.get("used"), {"queries", "rows", "db_ms"}, f"{label}.gateway used")
    reserved = _audit_nonnegative_map(native.get("reserved"), {"queries", "rows", "db_ms"}, f"{label}.gateway reserved")
    require(all(used[field] <= limits[field] for field in ("queries", "rows", "db_ms")), f"{label}.gateway native budget exceeded")
    require(
        {field: limits[field] for field in GATEWAY_AUDIT_NATIVE_LIMITS} == GATEWAY_AUDIT_NATIVE_LIMITS,
        f"{label}.gateway native limits differ from the registered audit ceiling",
    )
    require(all(reserved[field] == 0 for field in ("queries", "rows", "db_ms")), f"{label}.gateway budget retains reservations")
    exposure = snapshot.get("exposure_budget")
    require(isinstance(exposure, dict), f"{label}.gateway exposure audit is missing")
    exposure_fields = {"release_facts", "influence_facts", "outcome_facts"}
    exposure_limits = _audit_nonnegative_map(exposure.get("limits"), exposure_fields, f"{label}.exposure limits")
    exposure_used = _audit_nonnegative_map(exposure.get("used"), exposure_fields, f"{label}.exposure used")
    require(
        all(exposure_used[field] <= exposure_limits[field] for field in exposure_fields),
        f"{label}.gateway exposure budget exceeded",
    )
    observed_facts = record["common_v3_risk"]
    observed_native = record["native_usage"]
    if record["arm"] in {"taskgate_v3", "unlimited"}:
        require(
            all(observed_facts[field] == exposure_used[field] for field in exposure_fields),
            f"{label}.gateway exposure usage differs from admitted Fact evidence",
        )
    else:
        require(
            all(observed_facts[field] <= exposure_used[field] for field in exposure_fields),
            f"{label}.admitted Fact evidence exceeds gateway audit usage",
        )
    # The gateway ledger is an operational charge ledger, whereas native_usage
    # is reconstructed from responses actually admitted to the Agent.  A
    # baseline controller may withhold a successfully completed gateway result.
    # More subtly, TaskGate can execute a query and charge its query/row cost,
    # then atomically reject the result when its novel exposure would exceed the
    # semantic budget.  Consequently operational usage may exceed disclosed
    # usage in every arm, but never the reverse.
    require(
        observed_native["successful_queries"] <= used["queries"]
        and observed_native["returned_rows"] <= used["rows"],
        f"{label}.admitted native usage exceeds gateway audit usage",
    )
    require(
        used["queries"] <= len(record["queries"]),
        f"{label}.gateway query usage exceeds the execute_plan trace",
    )
    if record["arm"] == "taskgate_v3":
        expected_limits = record["budget"]
        require(exposure_limits == expected_limits, f"{label}.TaskGate gateway limits differ from the study budget")
    else:
        require(
            {field: exposure_limits[field] for field in GATEWAY_AUDIT_EXPOSURE_LIMITS}
            == GATEWAY_AUDIT_EXPOSURE_LIMITS,
            f"{label}.gateway exposure limits differ from the registered audit ceiling",
        )


def _validate_budget_compliance(record: dict, label: str) -> None:
    arm = record["arm"]
    if arm == "unlimited":
        require(record["runtime_budget_rejections"] == 0, f"{label}.unlimited arm encountered a budget rejection")
        return
    for unit, path in USAGE_UNIT_MAPPING[arm].items():
        section, field = path.split(".")
        require(record[section][field] <= record["budget"][unit], f"{label}.{arm} exceeded its registered budget")


def _answer_roots(task: dict) -> set[str]:
    if "required_answer_fields" in task:
        return set(task["required_answer_fields"])
    return {
        item["answer_path"].split(".")[0]
        for item in task["rubric"]
        if item.get("answer_path")
    }


def _validate_provider_api(record: dict, lock: dict, label: str) -> None:
    provider = record.get("provider_api")
    require(isinstance(provider, dict), f"provider API audit is missing in {label}")
    for field in ("model_turns", "request_attempts", "successful_responses", "retry_attempts"):
        require(
            isinstance(provider.get(field), int) and not isinstance(provider[field], bool)
            and provider[field] >= 0,
            f"invalid provider API {field} in {label}",
        )
    require(
        provider["model_turns"] <= lock["max_tool_turns"]
        and provider["request_attempts"] <= lock["max_tool_turns"] * lock["api_retry"]["max_attempts"]
        and provider["model_turns"] <= provider["successful_responses"] <= provider["request_attempts"]
        and provider["retry_attempts"] == provider["request_attempts"] - provider["model_turns"],
        f"provider API attempt accounting is inconsistent in {label}",
    )
    usage = provider.get("token_usage")
    _nonnegative_int_fields(usage, PROVIDER_TOKEN_USAGE_FIELDS, f"{label}.provider_api.token_usage")
    require(
        usage["prompt_tokens"] == usage["prompt_cache_hit_tokens"] + usage["prompt_cache_miss_tokens"]
        and usage["total_tokens"] == usage["prompt_tokens"] + usage["completion_tokens"]
        and usage["reasoning_tokens"] <= usage["completion_tokens"],
        f"provider token accounting is inconsistent in {label}",
    )
    if lock["thinking_mode"] == "disabled":
        require(usage["reasoning_tokens"] == 0, f"non-thinking run reported reasoning tokens in {label}")
    fingerprints = provider.get("system_fingerprints")
    reasons = provider.get("finish_reasons")
    require(
        isinstance(fingerprints, list)
        and fingerprints == sorted(set(fingerprints))
        and all(isinstance(value, str) and value for value in fingerprints)
        and isinstance(reasons, list)
        and len(reasons) == provider["successful_responses"]
        and all(isinstance(value, str) and value for value in reasons),
        f"provider response metadata is inconsistent in {label}",
    )
    require(
        (provider["successful_responses"] == 0 and not fingerprints)
        or (provider["successful_responses"] > 0 and bool(fingerprints)),
        f"provider fingerprint coverage is inconsistent in {label}",
    )
    expected_cost = (
        usage["prompt_cache_hit_tokens"] * lock["pricing_usd_per_million_tokens"]["prompt_cache_hit"]
        + usage["prompt_cache_miss_tokens"] * lock["pricing_usd_per_million_tokens"]["prompt_cache_miss"]
        + usage["completion_tokens"] * lock["pricing_usd_per_million_tokens"]["completion"]
    ) / 1_000_000
    cost = provider.get("estimated_cost_usd")
    require(
        isinstance(cost, (int, float)) and not isinstance(cost, bool)
        and math.isfinite(float(cost)) and cost >= 0
        and math.isclose(float(cost), expected_cost, rel_tol=0, abs_tol=1e-12),
        f"provider cost estimate is inconsistent in {label}",
    )


def _validate_run_record(
    record: dict,
    path: Path,
    task: dict,
    protocol: dict,
    lock_sha: str,
    lock: dict,
    schedule_kind: str,
) -> None:
    label = path.name
    require(record.get("schema_version") == 3, f"unsupported run record {label}")
    require(record.get("study_id") == protocol["study_id"], f"study identity mismatch in {label}")
    require(record.get("task_id") == task["id"] and record.get("domain") == task["domain"], f"task/domain mismatch in {label}")
    require(record.get("arm") in ARMS, f"unknown arm in {label}")
    require(isinstance(record.get("replicate"), int) and not isinstance(record["replicate"], bool), f"invalid replicate in {label}")
    require(record["replicate"] >= 0, f"negative replicate in {label}")
    level = record.get("budget_level")
    require(
        isinstance(level, (int, float)) and not isinstance(level, bool) and math.isfinite(float(level)),
        f"invalid budget level in {label}",
    )
    require("seed" not in record and "budget_multiplier" not in record, f"legacy seed/multiplier fields in {label}")
    require(record.get("database_snapshot") == "workflow-study-2026-v1", f"wrong database snapshot in {label}")
    require(record.get("status") in {"completed", "budget_exhausted", "tool_error", "agent_error"}, f"invalid status in {label}")
    started = timestamp(record.get("started_at"), f"{label}.started_at")
    finished = timestamp(record.get("finished_at"), f"{label}.finished_at")
    require(finished > started, f"invalid run duration in {label}")
    if schedule_kind == "calibration":
        require(
            started > timestamp(lock.get("locked_at"), "execution lock locked_at"),
            f"calibration run predates its execution lock in {label}",
        )
    require(record.get("execution_lock_sha256") == lock_sha, f"execution lock differs in {label}")
    expected_model = {
        "provider": lock["provider"],
        "model": lock["model"],
        "version": lock["model_version"],
        "thinking_mode": lock["thinking_mode"],
        "temperature": lock["temperature"],
        "top_p": lock["top_p"],
        "max_tokens": lock["max_tokens"],
        "api_base_url": lock["api_base_url"],
    }
    require(record.get("model") == expected_model, f"model configuration differs from execution lock in {label}")
    provider_models = record.get("provider_response_models")
    require(
        isinstance(provider_models, list)
        and all(isinstance(model, str) for model in provider_models)
        and provider_models == sorted(set(provider_models))
        and all(model == lock["model"] for model in provider_models),
        f"provider response model differs from execution lock in {label}",
    )
    _validate_provider_api(record, lock, label)
    require(record.get("budget_rejection_envelope") == "taskgate-study-budget-rejection-v1", f"rejection envelope differs in {label}")
    for field in ("run_id", "root_task_id", "database_instance_id", "cache_namespace"):
        require(isinstance(record.get(field), str) and record[field] and "replace" not in record[field], f"invalid {field} in {label}")
    expected_run_id = registered_run_id(
        protocol["study_id"], schedule_kind, task["id"], record["arm"], record["replicate"], float(level),
    )
    require(record["run_id"] == expected_run_id, f"run identity is not the registered schedule cell in {label}")
    require(record["cache_namespace"] == expected_run_id, f"cache namespace differs from registered run identity in {label}")
    require(isinstance(record.get("queries"), list), f"queries are missing in {label}")
    require(isinstance(record.get("final_answer"), dict), f"final answer is missing in {label}")
    require(isinstance(record.get("final_answer_text"), str), f"final answer narrative is invalid in {label}")
    if record["status"] == "completed":
        require(
            set(record["final_answer"]) == _answer_roots(task),
            f"completed final answer violates the locked field contract in {label}",
        )
        require("failure" not in record, f"completed record contains failure metadata in {label}")
    else:
        require(record["final_answer"] == {}, f"failed run must have an empty structured answer in {label}")
        if record["status"] in {"tool_error", "agent_error"}:
            failure = record.get("failure")
            require(
                isinstance(failure, dict) and isinstance(failure.get("category"), str) and failure["category"],
                f"terminal adapter failure lacks a category in {label}",
            )
    _nonnegative_int_fields(record.get("common_v3_risk"), COMMON_RISK_FIELDS, f"{label}.common_v3_risk")
    _nonnegative_int_fields(record.get("neutral_disclosure"), NEUTRAL_DISCLOSURE_FIELDS, f"{label}.neutral_disclosure")
    _nonnegative_int_fields(record.get("native_usage"), NATIVE_USAGE_FIELDS, f"{label}.native_usage")
    _nonnegative_int_fields(record.get("performance"), PERFORMANCE_FIELDS, f"{label}.performance")
    require(
        isinstance(record.get("runtime_budget_rejections"), int)
        and not isinstance(record["runtime_budget_rejections"], bool)
        and record["runtime_budget_rejections"] >= 0,
        f"invalid runtime rejection count in {label}",
    )
    require(
        isinstance(record.get("final_format_repair_attempts"), int)
        and not isinstance(record["final_format_repair_attempts"], bool)
        and 0 <= record["final_format_repair_attempts"] <= record["provider_api"]["model_turns"],
        f"invalid final-format repair count in {label}",
    )
    require(
        isinstance(record.get("compose_start_attempts"), int)
        and not isinstance(record["compose_start_attempts"], bool)
        and 1 <= record["compose_start_attempts"]
        <= lock["infrastructure_retry"]["compose_start_max_attempts"],
        f"invalid Compose-start attempt count in {label}",
    )
    recomputed_usage, admitted_query_ids, recomputed_rejections = _validate_admitted_responses(record, label)
    require(record["native_usage"] == recomputed_usage, f"{label}.native_usage is not response-derived")
    require(
        record["runtime_budget_rejections"] == recomputed_rejections,
        f"{label}.runtime rejection count is not trace-derived",
    )
    _validate_fact_evidence(record, task, admitted_query_ids, label)
    _validate_gateway_budget_audit(record, label)
    _validate_budget_compliance(record, label)
    if record["status"] == "completed":
        require(provider_models == [lock["model"]], f"completed run lacks a locked provider response model in {label}")
        require(record["provider_api"]["model_turns"] >= 1, f"completed run lacks provider usage in {label}")


def _require_fresh(records: list[tuple[Path, dict]]) -> None:
    for field in ("run_id", "root_task_id", "database_instance_id", "cache_namespace"):
        values = [record[field] for _, record in records]
        require(len(values) == len(set(values)), f"run collection reuses {field}")


def _complete_calibration_answer_value(value: object) -> bool:
    if value is None:
        return False
    if isinstance(value, str):
        return bool(value)
    # Zero, false, and empty collections can be legitimate aggregate or empty-result answers.
    return True


def validate_calibration_runs(
    directory: Path,
    calibration_doc: dict,
    protocol: dict,
    execution_lock_sha256: str,
    execution_lock: dict,
) -> list[dict]:
    plan = sampling_plan(protocol)
    tasks = {task["id"]: task for task in calibration_doc["tasks"]}
    expected = {(task_id, replicate) for task_id in tasks for replicate in range(plan["calibration_replicates"])}
    observed: set[tuple[str, int]] = set()
    records: list[tuple[Path, dict]] = []
    for path in json_files(directory):
        record = load(path)
        task_id = record.get("task_id")
        require(task_id in tasks, f"unknown calibration task in {path.name}")
        _validate_run_record(
            record, path, tasks[task_id], protocol, execution_lock_sha256, execution_lock, "calibration",
        )
        require(record.get("arm") == "unlimited", f"calibration must be unlimited in {path.name}")
        require(record.get("phase") == "algorithmic_calibration", f"wrong calibration phase in {path.name}")
        require(record.get("budget_level") == 0, f"calibration budget level must be zero in {path.name}")
        require(record.get("budget") == {}, f"calibration must have no study ceiling in {path.name}")
        require(record.get("algorithmic_budget_freeze_sha256") == ZERO_SHA256, f"calibration cannot consume a freeze in {path.name}")
        require(record.get("status") == "completed", f"calibration trace is not completed in {path.name}")
        require(
            record["native_usage"]["successful_queries"] >= 1,
            f"calibration trace has no admitted query in {path.name}",
        )
        require(
            all(_complete_calibration_answer_value(record["final_answer"][field]) for field in tasks[task_id]["required_answer_fields"]),
            f"calibration trace has an incomplete answer in {path.name}",
        )
        cell = (task_id, record["replicate"])
        require(cell not in observed, f"duplicate calibration cell in {path.name}")
        observed.add(cell)
        records.append((path, record))
    require(observed == expected, f"calibration coverage differs: {len(expected - observed)} missing, {len(observed - expected)} extra")
    _require_fresh(records)
    return [record for _, record in records]


def validate_runs(
    directory: Path,
    tasks_doc: dict,
    protocol: dict,
    frozen: dict,
    execution_lock_sha256: str,
    execution_lock: dict,
) -> list[dict]:
    plan = sampling_plan(protocol)
    frozen_at = timestamp(frozen.get("frozen_at"), "frozen_at")
    tasks = {task["id"]: task for task in tasks_doc["tasks"]}
    expected = {
        (task_id, arm, replicate, "budget_level", level)
        for task_id in tasks
        for arm in PRIMARY_ARMS
        for level in BUDGET_LEVELS
        for replicate in range(plan["evaluation_replicates_per_level"])
    }
    expected.update(
        (task_id, "unlimited", replicate, "unbudgeted_reference", 0.0)
        for task_id in tasks
        for replicate in range(plan["unlimited_replicates"])
    )
    observed: set[tuple[str, str, int, str, float]] = set()
    records: list[tuple[Path, dict]] = []
    for path in json_files(directory):
        record = load(path)
        task_id = record.get("task_id")
        require(task_id in tasks, f"unknown evaluation task in {path.name}")
        _validate_run_record(
            record, path, tasks[task_id], protocol, execution_lock_sha256, execution_lock, "evaluation",
        )
        require(
            timestamp(record["started_at"], f"{path.name}.started_at") > frozen_at,
            f"evaluation run does not postdate the algorithmic freeze in {path.name}",
        )
        arm = record["arm"]
        level = float(record.get("budget_level", -1))
        phase = record.get("phase")
        if arm == "unlimited":
            require(phase == "unbudgeted_reference" and level == 0 and record.get("budget") == {}, f"invalid unlimited cell in {path.name}")
        else:
            require(phase == "budget_level" and level in BUDGET_LEVELS, f"invalid budgeted cell in {path.name}")
            expected_budget = frozen["domains"][tasks[task_id]["domain"]]["levels"][level_key(level)][arm]
            require(record.get("budget") == expected_budget, f"run budget differs from frozen domain level in {path.name}")
            validate_budget(record["budget"], arm, f"{path.name}.budget")
        require(record.get("algorithmic_budget_freeze_sha256") == frozen["freeze_sha256"], f"freeze differs in {path.name}")
        cell = (task_id, arm, record["replicate"], phase, level)
        require(cell not in observed, f"duplicate evaluation cell in {path.name}")
        observed.add(cell)
        records.append((path, record))
    require(observed == expected, f"evaluation coverage differs: {len(expected - observed)} missing, {len(observed - expected)} extra")
    require(len(observed) == plan["planned_evaluation_runs"], "evaluation run count differs from registration")
    _require_fresh(records)
    return [record for _, record in records]


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--truth", type=Path)
    parser.add_argument("--calibration-runs", type=Path)
    parser.add_argument("--freeze", type=Path)
    parser.add_argument("--execution-lock", type=Path)
    parser.add_argument("--runs", type=Path)
    args = parser.parse_args()
    try:
        tasks_doc, calibration_doc, protocol = validate_design()
        if args.truth:
            validate_truth(args.truth, {task["id"] for task in tasks_doc["tasks"]}, tasks_doc)
        lock_sha = None
        lock = None
        if args.execution_lock:
            lock = validate_execution_lock(args.execution_lock, protocol["study_id"])
            lock_sha = file_sha256(args.execution_lock)
        if args.calibration_runs:
            require(lock_sha is not None and lock is not None, "--calibration-runs requires --execution-lock")
            validate_calibration_runs(args.calibration_runs, calibration_doc, protocol, lock_sha, lock)
        frozen = None
        if args.freeze:
            require(args.execution_lock is not None and args.calibration_runs is not None, "--freeze requires --execution-lock and --calibration-runs")
            frozen = validate_algorithmic_freeze(args.freeze, protocol, args.execution_lock, args.calibration_runs)
        if args.runs:
            require(frozen is not None and lock_sha is not None and lock is not None, "--runs requires --freeze and --execution-lock")
            validate_runs(args.runs, tasks_doc, protocol, frozen, lock_sha, lock)
    except ValueError as error:
        raise SystemExit(f"workflow-study validation failed: {error}") from error
    plan = sampling_plan(protocol)
    print(
        "ok - controlled workflow benchmark: "
        f"{plan['calibration_tasks']} calibration tasks/{plan['calibration_runs']} traces, "
        f"{plan['evaluation_tasks']} evaluation tasks/{plan['planned_evaluation_runs']} runs"
    )


if __name__ == "__main__":
    main()
