#!/usr/bin/env python3
"""Certify one complete controlled-workflow collection for publication.

The certificate is deliberately separate from the frozen protocol and raw
results.  It is emitted only after the registered schedules and all 654 raw
records validate, ``analyze.py`` reproduces the candidate JSON and CSV byte
for byte in a temporary directory, and none of the inputs changes while those
checks run.

The certifier also fixes the reanalysis subprocess to ``PYTHONHASHSEED=0`` as
defense in depth for a single canonical JSON-object and CSV-column order.

``collection_complete`` means that every registered, auditable collection
cell is present.  It does not mean that every Agent workflow completed or
answered correctly; those counts are reported separately in the certificate.
"""

from __future__ import annotations

import argparse
import csv
import datetime as dt
import hashlib
import io
import json
import os
import subprocess
import sys
import tempfile
from collections import Counter, defaultdict
from pathlib import Path

import analyze
import run_study
import validate


HERE = Path(__file__).resolve().parent
ALLOWED_EVALUATION_STATUSES = ("completed", "budget_exhausted", "tool_error", "agent_error")
ANALYSIS_PYTHON_HASH_SEED = "0"


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def _read_required_bytes(path: Path, label: str) -> bytes:
    require(path.is_file(), f"{label} is missing: {path}")
    try:
        return path.read_bytes()
    except OSError as error:
        raise ValueError(f"cannot read {label} {path}: {error}") from error


def _sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def _file_binding(path: Path, label: str) -> dict:
    value = _read_required_bytes(path, label)
    return {"name": path.name, "bytes": len(value), "sha256": _sha256_bytes(value)}


def _record_manifest(directory: Path, expected: int, label: str) -> list[dict]:
    require(directory.is_dir(), f"{label} directory is missing: {directory}")
    # Formal run directories have no templates.  Reject every extra JSON,
    # including names containing ".example.", instead of inheriting the
    # design-time convenience filter used by validate.json_files().
    paths = sorted(directory.glob("*.json"))
    require(
        len(paths) == expected,
        f"{label} manifest has {len(paths)} JSON files; expected exactly {expected}",
    )
    manifest = [_file_binding(path, f"{label} record") for path in paths]
    require(
        [item["name"] for item in manifest] == sorted(item["name"] for item in manifest),
        f"{label} manifest is not filename-sorted",
    )
    require(
        len({item["name"] for item in manifest}) == expected,
        f"{label} manifest repeats a filename",
    )
    return manifest


def _fixed_artifact_bindings(
    *,
    truth: Path,
    freeze: Path,
    execution_lock: Path,
    calibration_schedule: Path,
    evaluation_schedule: Path,
) -> dict:
    paths = {
        "evaluation_tasks": validate.TASKS,
        "calibration_tasks": validate.CALIBRATION_TASKS,
        "protocol": validate.PROTOCOL,
        "truth": truth,
        "execution_lock": execution_lock,
        "algorithmic_budget_freeze": freeze,
        "calibration_schedule": calibration_schedule,
        "evaluation_schedule": evaluation_schedule,
        "validator": HERE / "validate.py",
        "schedule_validator": HERE / "run_study.py",
        "analyzer": HERE / "analyze.py",
        "collection_certifier": Path(__file__).resolve(),
    }
    return {name: _file_binding(path, name.replace("_", " ")) for name, path in paths.items()}


def _collection_snapshot(
    *,
    plan: dict,
    truth: Path,
    freeze: Path,
    execution_lock: Path,
    calibration_schedule: Path,
    evaluation_schedule: Path,
    calibration_runs: Path,
    evaluation_runs: Path,
) -> dict:
    """Hash every certification input to detect in-flight replacement."""
    return {
        "fixed_artifacts": _fixed_artifact_bindings(
            truth=truth,
            freeze=freeze,
            execution_lock=execution_lock,
            calibration_schedule=calibration_schedule,
            evaluation_schedule=evaluation_schedule,
        ),
        "calibration_run_records": _record_manifest(
            calibration_runs, plan["calibration_runs"], "calibration run",
        ),
        "evaluation_run_records": _record_manifest(
            evaluation_runs, plan["planned_evaluation_runs"], "evaluation run",
        ),
    }


def _load_json_object(path: Path, label: str) -> dict:
    value = _read_required_bytes(path, label)
    try:
        document = json.loads(value.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError(f"{label} is not valid UTF-8 JSON: {error}") from error
    require(isinstance(document, dict), f"{label} must be a JSON object")
    return document


def _record_ids(records: list[dict], expected: int, label: str) -> set[str]:
    run_ids = [record.get("run_id") for record in records]
    require(
        len(run_ids) == expected
        and all(isinstance(run_id, str) and run_id for run_id in run_ids)
        and len(set(run_ids)) == expected,
        f"{label} does not contain exactly {expected} unique run IDs",
    )
    return set(run_ids)


def _schedule_ids(schedule: dict, expected: int, label: str) -> set[str]:
    cells = schedule.get("cells")
    require(isinstance(cells, list) and len(cells) == expected, f"{label} has the wrong cell count")
    run_ids = [cell.get("run_id") for cell in cells if isinstance(cell, dict)]
    require(
        len(run_ids) == expected
        and all(isinstance(run_id, str) and run_id for run_id in run_ids)
        and len(set(run_ids)) == expected,
        f"{label} does not contain exactly {expected} unique run IDs",
    )
    return set(run_ids)


def _phase_execution_audit(
    schedule: dict,
    records: list[dict],
    lock: dict,
    phase: str,
) -> dict:
    """Validate registered execution order and the locked phase-cost ceiling."""
    by_run_id = {record["run_id"]: record for record in records}
    cells = schedule["cells"]
    limit = lock.get("phase_cost_limits_usd", {}).get(phase)
    require(
        isinstance(limit, (int, float)) and not isinstance(limit, bool) and float(limit) > 0,
        f"execution lock lacks a positive {phase} phase-cost ceiling",
    )
    cumulative_cost = 0.0
    prior_finished = None
    first_started = None
    last_finished = None
    for position, cell in enumerate(cells):
        record = by_run_id[cell["run_id"]]
        started = validate.timestamp(record.get("started_at"), f"{phase} sequence {position} started_at")
        finished = validate.timestamp(record.get("finished_at"), f"{phase} sequence {position} finished_at")
        require(finished > started, f"{phase} run has a non-positive duration at sequence {position}")
        if prior_finished is not None:
            require(
                started >= prior_finished,
                f"{phase} run timestamps do not follow the registered schedule at sequence {position}",
            )
        provider = record.get("provider_api")
        cost = provider.get("estimated_cost_usd") if isinstance(provider, dict) else None
        require(
            isinstance(cost, (int, float)) and not isinstance(cost, bool) and float(cost) >= 0,
            f"{phase} run {record['run_id']} lacks provider cost evidence",
        )
        cumulative_cost += float(cost)
        require(
            cumulative_cost <= float(limit),
            f"{phase} cumulative provider cost exceeds its locked ceiling at sequence {position}",
        )
        first_started = first_started or record["started_at"]
        last_finished = record["finished_at"]
        prior_finished = finished
    return {
        "registered_schedule_timestamp_order_verified": True,
        "first_started_at": first_started,
        "last_finished_at": last_finished,
        "cost_limit_usd": float(limit),
        "observed_cost_usd": cumulative_cost,
        "remaining_cost_headroom_usd": float(limit) - cumulative_cost,
    }


def _validate_registered_collection(
    *,
    truth: Path,
    freeze_path: Path,
    execution_lock_path: Path,
    calibration_schedule_path: Path,
    evaluation_schedule_path: Path,
    calibration_runs_path: Path,
    evaluation_runs_path: Path,
) -> tuple[dict, dict, dict, dict, dict, list[dict], list[dict], dict]:
    """Run every existing source-registered validator, including schedules."""
    design = validate.validate_design()
    require(isinstance(design, tuple) and len(design) == 3, "validate_design returned an invalid contract")
    tasks_doc, calibration_doc, protocol = design
    plan = validate.sampling_plan(protocol)
    require(
        plan["calibration_runs"] == 18
        and plan["planned_evaluation_runs"] == 636
        and plan["total_agent_runs"] == 654,
        "certification requires the registered 18 + 636 = 654 design",
    )

    validate.validate_truth(truth, {task["id"] for task in tasks_doc["tasks"]}, tasks_doc)
    lock = validate.validate_execution_lock(execution_lock_path, protocol["study_id"])
    lock_sha = validate.file_sha256(execution_lock_path)

    calibration_schedule = validate.load(calibration_schedule_path)
    run_study.validate_schedule(calibration_schedule, calibration_doc, protocol, lock_sha)
    calibration_records = validate.validate_calibration_runs(
        calibration_runs_path, calibration_doc, protocol, lock_sha, lock,
    )

    frozen = validate.validate_algorithmic_freeze(
        freeze_path, protocol, execution_lock_path, calibration_runs_path,
    )
    require(
        validate.file_sha256(truth) == frozen["source_file_sha256"]["raw/ground-truth.json"],
        "certification truth differs from the truth bound into the algorithmic freeze",
    )
    evaluation_schedule = validate.load(evaluation_schedule_path)
    run_study.validate_schedule(evaluation_schedule, tasks_doc, protocol, lock_sha, frozen)
    evaluation_records = validate.validate_runs(
        evaluation_runs_path, tasks_doc, protocol, frozen, lock_sha, lock,
    )

    calibration_ids = _record_ids(calibration_records, plan["calibration_runs"], "calibration records")
    evaluation_ids = _record_ids(evaluation_records, plan["planned_evaluation_runs"], "evaluation records")
    require(
        calibration_ids == _schedule_ids(calibration_schedule, plan["calibration_runs"], "calibration schedule"),
        "calibration records do not match the certified schedule",
    )
    require(
        evaluation_ids == _schedule_ids(evaluation_schedule, plan["planned_evaluation_runs"], "evaluation schedule"),
        "evaluation records do not match the certified schedule",
    )
    require(calibration_ids.isdisjoint(evaluation_ids), "calibration and evaluation reuse a run ID")
    return (
        tasks_doc, protocol, plan, lock, frozen, calibration_records, evaluation_records,
        {"calibration": calibration_schedule, "evaluation": evaluation_schedule},
    )


def rerun_analysis(
    *,
    truth: Path,
    freeze: Path,
    calibration_runs: Path,
    execution_lock: Path,
    evaluation_runs: Path,
    output: Path,
    scored_csv: Path,
    timeout_seconds: int,
) -> None:
    """Execute the frozen analyzer as a fresh process without any Agent/API call."""
    command = [
        sys.executable,
        str(HERE / "analyze.py"),
        "--truth", str(truth),
        "--freeze", str(freeze),
        "--calibration-runs", str(calibration_runs),
        "--execution-lock", str(execution_lock),
        "--runs", str(evaluation_runs),
        "--output", str(output),
        "--scored-csv", str(scored_csv),
    ]
    try:
        environment = dict(os.environ)
        # Keep interpreter hash iteration deterministic even if a future
        # analyzer implementation introduces an unordered container.
        environment["PYTHONHASHSEED"] = ANALYSIS_PYTHON_HASH_SEED
        # Certification is an offline operation.  Do not give the analyzer a
        # provider credential even if the caller's shell has one configured.
        environment.pop("DEEPSEEK_API_KEY", None)
        completed = subprocess.run(
            command,
            cwd=HERE,
            env=environment,
            text=True,
            capture_output=True,
            timeout=timeout_seconds,
            check=False,
        )
    except subprocess.TimeoutExpired as error:
        raise ValueError(f"deterministic reanalysis timed out after {timeout_seconds} seconds") from error
    if completed.returncode != 0:
        detail = (completed.stderr or completed.stdout).strip()
        if len(detail) > 2000:
            detail = detail[-2000:]
        raise ValueError(
            f"deterministic reanalysis failed with exit {completed.returncode}: {detail or 'no diagnostic'}"
        )
    require(output.is_file(), "deterministic reanalysis did not produce results JSON")
    require(scored_csv.is_file(), "deterministic reanalysis did not produce scored CSV")


def _validate_candidate_result(
    *,
    result: dict,
    candidate_csv: bytes,
    protocol: dict,
    plan: dict,
    lock: dict,
    lock_sha: str,
    frozen: dict,
    truth_sha: str,
    evaluation_records: list[dict],
) -> list[dict]:
    expected = plan["planned_evaluation_runs"]
    require(result.get("schema_version") == 3, "candidate results use an unsupported schema")
    require(result.get("study_id") == protocol["study_id"], "candidate results belong to another study")
    require(
        result.get("status") == "complete_registered_collection",
        "candidate results do not claim the analyzer's complete registered collection status",
    )
    require(result.get("evaluation_runs") == expected, "candidate results do not contain all 636 evaluation runs")
    result_lock = result.get("execution_lock")
    require(
        isinstance(result_lock, dict)
        and result_lock.get("sha256") == lock_sha
        and result_lock.get("provider") == lock["provider"]
        and result_lock.get("model") == lock["model"]
        and result_lock.get("model_version") == lock["model_version"]
        and result_lock.get("thinking_mode") == lock["thinking_mode"],
        "candidate results differ from the certified execution lock",
    )
    require(
        result.get("algorithmic_budget_freeze_sha256") == frozen["freeze_sha256"],
        "candidate results differ from the certified budget freeze",
    )
    scoring = result.get("scoring")
    require(isinstance(scoring, dict), "candidate results lack scoring provenance")
    require(
        scoring.get("type") == "deterministic_policy_blind_automatic"
        and scoring.get("human_or_llm_judge_inputs") is False,
        "candidate results do not use the registered automatic scoring contract",
    )
    require(scoring.get("task_manifest_sha256") == validate.file_sha256(validate.TASKS), "candidate task digest differs")
    require(scoring.get("truth_sha256") == truth_sha, "candidate truth digest differs")
    require(scoring.get("analysis_sha256") == validate.file_sha256(HERE / "analyze.py"), "candidate analyzer digest differs")

    scored = result.get("scored_runs")
    require(isinstance(scored, list) and len(scored) == expected, "candidate scored_runs is not the complete 636-run collection")
    scored_ids = _record_ids(scored, expected, "candidate scored runs")
    raw_by_id = {record["run_id"]: record for record in evaluation_records}
    require(scored_ids == set(raw_by_id), "candidate scored run IDs differ from the raw evaluation records")
    scored_by_id = {row["run_id"]: row for row in scored}
    identity_fields = ("task_id", "arm", "replicate", "phase", "status")
    for run_id, record in raw_by_id.items():
        row = scored_by_id[run_id]
        require(
            all(row.get(field) == record.get(field) for field in identity_fields)
            and float(row.get("budget_level", -1)) == float(record.get("budget_level", -2)),
            f"candidate scored identity differs from raw record {run_id}",
        )

    try:
        csv_text = candidate_csv.decode("utf-8")
        csv_rows = list(csv.DictReader(io.StringIO(csv_text, newline="")))
    except (UnicodeDecodeError, csv.Error) as error:
        raise ValueError(f"candidate scored CSV is invalid: {error}") from error
    require(len(csv_rows) == expected, "candidate scored CSV does not contain exactly 636 data rows")
    csv_ids = [row.get("run_id") for row in csv_rows]
    require(
        all(csv_ids) and len(set(csv_ids)) == expected and set(csv_ids) == scored_ids,
        "candidate scored CSV run IDs differ from candidate results",
    )
    return scored


def _status_counts(records: list[dict]) -> dict[str, int]:
    observed = Counter(record["status"] for record in records)
    require(set(observed).issubset(ALLOWED_EVALUATION_STATUSES), "evaluation records contain an unknown status")
    return {status: observed.get(status, 0) for status in ALLOWED_EVALUATION_STATUSES}


def _arm_level_counts(records: list[dict]) -> dict[str, dict[str, int]]:
    counts: dict[str, Counter] = defaultdict(Counter)
    for record in records:
        arm = record["arm"]
        key = "unbudgeted_reference" if arm == "unlimited" else validate.level_key(record["budget_level"])
        counts[arm][key] += 1
    return {
        arm: dict(sorted(levels.items()))
        for arm, levels in sorted(counts.items())
    }


def _expected_arm_level_counts(plan: dict) -> dict[str, dict[str, int]]:
    per_budget_cell = plan["evaluation_tasks"] * plan["evaluation_replicates_per_level"]
    return {
        **{
            arm: {validate.level_key(level): per_budget_cell for level in validate.BUDGET_LEVELS}
            for arm in sorted(validate.PRIMARY_ARMS)
        },
        "unlimited": {
            "unbudgeted_reference": plan["evaluation_tasks"] * plan["unlimited_replicates"],
        },
    }


def _binary_score_count(scored: list[dict], field: str) -> int:
    values = [row.get(field) for row in scored]
    require(
        all(isinstance(value, (int, float)) and not isinstance(value, bool) and float(value) in {0.0, 1.0} for value in values),
        f"candidate {field} values are not binary",
    )
    return int(sum(float(value) for value in values))


def _estimability_summary(result: dict) -> dict:
    benchmark = result.get("automatic_quality_exposure_benchmark")
    require(isinstance(benchmark, dict), "candidate results lack the registered benchmark analysis")
    quality = benchmark.get("quality_exposure_auc")
    completion = benchmark.get("critical_completion_at_matched_exposure")
    require(isinstance(quality, dict) and isinstance(completion, dict), "candidate results lack estimability summaries")
    return {
        "quality_auc_tasks_total": quality.get("tasks_total"),
        "quality_auc_tasks_estimable": quality.get("tasks_with_common_support"),
        "quality_auc_unestimable_task_ids": quality.get("unestimable_task_ids"),
        "completion_tasks_total": completion.get("tasks_total"),
        "completion_tasks_estimable": completion.get("tasks_with_common_support"),
        "completion_unestimable_task_ids": completion.get("unestimable_task_ids"),
    }


def _utc_timestamp() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def _path_is_within(path: Path, directory: Path) -> bool:
    try:
        path.resolve().relative_to(directory.resolve())
    except ValueError:
        return False
    return True


def _validate_output_path(output: Path, inputs: list[Path], run_directories: list[Path]) -> None:
    resolved = output.resolve()
    require(all(resolved != path.resolve() for path in inputs), "certificate output must not overwrite an input artifact")
    require(
        all(not _path_is_within(output, directory) for directory in run_directories),
        "certificate output must be outside raw run-record directories",
    )


def certify_collection(
    *,
    truth: Path,
    freeze: Path,
    calibration_schedule: Path,
    calibration_runs: Path,
    evaluation_schedule: Path,
    execution_lock: Path,
    evaluation_runs: Path,
    results: Path,
    scored_csv: Path,
    analysis_timeout_seconds: int = 1800,
) -> dict:
    """Validate and return a self-digesting publication certificate payload."""
    require(
        isinstance(analysis_timeout_seconds, int)
        and not isinstance(analysis_timeout_seconds, bool)
        and 1 <= analysis_timeout_seconds <= 86400,
        "analysis timeout must be an integer from 1 to 86400 seconds",
    )

    # Validate the source registration first so the snapshot count is not
    # accepted from a caller-controlled result file.
    _, _, initial_protocol = validate.validate_design()
    initial_plan = validate.sampling_plan(initial_protocol)
    initial_snapshot = _collection_snapshot(
        plan=initial_plan,
        truth=truth,
        freeze=freeze,
        execution_lock=execution_lock,
        calibration_schedule=calibration_schedule,
        evaluation_schedule=evaluation_schedule,
        calibration_runs=calibration_runs,
        evaluation_runs=evaluation_runs,
    )
    candidate_results = _read_required_bytes(results, "candidate results")
    candidate_csv = _read_required_bytes(scored_csv, "candidate scored CSV")

    (
        tasks_doc, protocol, plan, lock, frozen, calibration_records,
        evaluation_records, schedules,
    ) = _validate_registered_collection(
        truth=truth,
        freeze_path=freeze,
        execution_lock_path=execution_lock,
        calibration_schedule_path=calibration_schedule,
        evaluation_schedule_path=evaluation_schedule,
        calibration_runs_path=calibration_runs,
        evaluation_runs_path=evaluation_runs,
    )
    del tasks_doc
    require(plan == initial_plan and protocol == initial_protocol, "registered design changed during certification")
    require(
        _collection_snapshot(
            plan=plan,
            truth=truth,
            freeze=freeze,
            execution_lock=execution_lock,
            calibration_schedule=calibration_schedule,
            evaluation_schedule=evaluation_schedule,
            calibration_runs=calibration_runs,
            evaluation_runs=evaluation_runs,
        ) == initial_snapshot,
        "collection inputs changed during validation",
    )
    phase_execution = {
        "calibration": _phase_execution_audit(
            schedules["calibration"], calibration_records, lock, "calibration",
        ),
        "evaluation": _phase_execution_audit(
            schedules["evaluation"], evaluation_records, lock, "evaluation",
        ),
    }

    try:
        candidate_result = json.loads(candidate_results.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError(f"candidate results are not valid UTF-8 JSON: {error}") from error
    require(isinstance(candidate_result, dict), "candidate results must be a JSON object")
    scored = _validate_candidate_result(
        result=candidate_result,
        candidate_csv=candidate_csv,
        protocol=protocol,
        plan=plan,
        lock=lock,
        lock_sha=validate.file_sha256(execution_lock),
        frozen=frozen,
        truth_sha=validate.file_sha256(truth),
        evaluation_records=evaluation_records,
    )

    with tempfile.TemporaryDirectory(prefix="workflow-collection-certification-") as temporary:
        temporary_root = Path(temporary)
        reproduced_results_path = temporary_root / "results.json"
        reproduced_csv_path = temporary_root / "scored-runs.csv"
        rerun_analysis(
            truth=truth,
            freeze=freeze,
            calibration_runs=calibration_runs,
            execution_lock=execution_lock,
            evaluation_runs=evaluation_runs,
            output=reproduced_results_path,
            scored_csv=reproduced_csv_path,
            timeout_seconds=analysis_timeout_seconds,
        )
        reproduced_results = _read_required_bytes(reproduced_results_path, "reproduced results")
        reproduced_csv = _read_required_bytes(reproduced_csv_path, "reproduced scored CSV")

    require(
        _collection_snapshot(
            plan=plan,
            truth=truth,
            freeze=freeze,
            execution_lock=execution_lock,
            calibration_schedule=calibration_schedule,
            evaluation_schedule=evaluation_schedule,
            calibration_runs=calibration_runs,
            evaluation_runs=evaluation_runs,
        ) == initial_snapshot,
        "collection inputs changed during deterministic reanalysis",
    )
    require(
        _read_required_bytes(results, "candidate results") == candidate_results
        and _read_required_bytes(scored_csv, "candidate scored CSV") == candidate_csv,
        "candidate result artifacts changed during certification",
    )
    require(
        reproduced_results == candidate_results,
        "candidate results are stale or differ from deterministic reanalysis",
    )
    require(
        reproduced_csv == candidate_csv,
        "candidate scored CSV is stale or differs from deterministic reanalysis",
    )

    status_counts = _status_counts(evaluation_records)
    arm_level_counts = _arm_level_counts(evaluation_records)
    calibration_status_counts = dict(sorted(Counter(record["status"] for record in calibration_records).items()))
    require(calibration_status_counts == {"completed": plan["calibration_runs"]}, "calibration collection is not fully completed")
    require(sum(status_counts.values()) == plan["planned_evaluation_runs"], "evaluation status counts do not sum to 636")
    require(
        arm_level_counts == _expected_arm_level_counts(plan),
        "evaluation arm/level counts differ from the registered 36-per-level and 60-reference design",
    )

    schedule_summary = {}
    for kind in ("calibration", "evaluation"):
        schedule = schedules[kind]
        schedule_summary[kind] = {
            **initial_snapshot["fixed_artifacts"][f"{kind}_schedule"],
            "declared_schedule_sha256": schedule["schedule_sha256"],
            "cells": len(schedule["cells"]),
        }

    result_binding = {"name": results.name, "bytes": len(candidate_results), "sha256": _sha256_bytes(candidate_results)}
    csv_binding = {"name": scored_csv.name, "bytes": len(candidate_csv), "sha256": _sha256_bytes(candidate_csv)}
    certificate = {
        "schema_version": 1,
        "study_id": protocol["study_id"],
        "certification_status": "validated_complete_registered_collection",
        "collection_complete": True,
        "collection_complete_semantics": (
            "All 18 calibration and 636 evaluation cells are present and auditable; "
            "this does not assert that every evaluation workflow completed or answered correctly."
        ),
        "integrity_scope": {
            "self_digest_is_not_a_signature": True,
            "external_signature_or_append_only_anchor": False,
            "downstream_verification": (
                "Rerun this certifier with --verify-existing against the current raw artifacts; "
                "do not trust manifest_sha256 alone."
            ),
        },
        "certified_at": _utc_timestamp(),
        "registered_counts": {
            "calibration_runs": plan["calibration_runs"],
            "evaluation_runs": plan["planned_evaluation_runs"],
            "total_agent_runs": plan["total_agent_runs"],
        },
        "calibration_status_counts": calibration_status_counts,
        "evaluation_status_counts": status_counts,
        "successful_workflows": {
            "execution_status_completed": status_counts["completed"],
            "execution_status_not_completed": plan["planned_evaluation_runs"] - status_counts["completed"],
            "automatic_answer_task_complete": _binary_score_count(scored, "answer_task_complete"),
            "automatic_workflow_task_complete": _binary_score_count(scored, "workflow_task_complete"),
        },
        "evaluation_arm_level_counts": arm_level_counts,
        "estimability": _estimability_summary(candidate_result),
        "campaign": {
            "campaign_id": lock["campaign_id"],
            "study_id": lock["study_id"],
            "locked_at": lock["locked_at"],
            "frozen_at": frozen["frozen_at"],
            "provider": lock["provider"],
            "model": lock["model"],
            "model_version": lock["model_version"],
            "thinking_mode": lock["thinking_mode"],
            "temperature": lock["temperature"],
            "top_p": lock["top_p"],
            "max_tokens": lock["max_tokens"],
            "request_timeout_seconds": lock["request_timeout_seconds"],
            "adapter_timeout_seconds": lock["adapter_timeout_seconds"],
            "max_tool_turns": lock["max_tool_turns"],
            "api_retry": lock["api_retry"],
            "infrastructure_retry": lock["infrastructure_retry"],
            "pricing_source": lock["pricing_source"],
            "pricing_usd_per_million_tokens": lock["pricing_usd_per_million_tokens"],
            "phase_cost_limits_usd": lock["phase_cost_limits_usd"],
            "container_images": lock["container_images"],
            "container_runtime": lock["container_runtime"],
        },
        "execution_lock": {
            **initial_snapshot["fixed_artifacts"]["execution_lock"],
            "campaign_id": lock["campaign_id"],
        },
        "algorithmic_budget_freeze": {
            **initial_snapshot["fixed_artifacts"]["algorithmic_budget_freeze"],
            "declared_freeze_sha256": frozen["freeze_sha256"],
            "status": frozen["status"],
        },
        "schedules": schedule_summary,
        "phase_execution_audit": phase_execution,
        "artifact_bindings": {
            **initial_snapshot,
            "frozen_source_file_sha256": frozen["source_file_sha256"],
        },
        "deterministic_analysis": {
            "analyzer_sha256": initial_snapshot["fixed_artifacts"]["analyzer"]["sha256"],
            "python_executable_version": sys.version,
            "python_hash_seed": ANALYSIS_PYTHON_HASH_SEED,
            "candidate_results": result_binding,
            "candidate_scored_csv": csv_binding,
            "reproduced_results_sha256": _sha256_bytes(reproduced_results),
            "reproduced_scored_csv_sha256": _sha256_bytes(reproduced_csv),
            "results_byte_for_byte_match": True,
            "scored_csv_byte_for_byte_match": True,
        },
    }
    certificate["manifest_sha256"] = validate.canonical_sha256(certificate)
    return certificate


def write_atomic_manifest(path: Path, payload: dict) -> None:
    """Publish the certificate only after every check has succeeded."""
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary_path: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            "w", encoding="utf-8", dir=path.parent, delete=False, prefix=f".{path.name}.",
        ) as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True, allow_nan=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
            temporary_path = Path(handle.name)
        os.replace(temporary_path, path)
        temporary_path = None
        try:
            directory_fd = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        except OSError:
            # The file replacement is still atomic on filesystems that do not
            # permit fsync on a directory descriptor.
            pass
    finally:
        if temporary_path is not None:
            try:
                temporary_path.unlink()
            except FileNotFoundError:
                pass


def verify_existing_manifest(path: Path, freshly_recomputed: dict) -> dict:
    """Verify a published certificate against a freshly rerun certification."""
    existing = _load_json_object(path, "existing collection certificate")
    claimed = existing.get("manifest_sha256")
    self_digest_payload = dict(existing)
    self_digest_payload.pop("manifest_sha256", None)
    require(
        isinstance(claimed, str) and claimed == validate.canonical_sha256(self_digest_payload),
        "existing collection certificate self-digest is invalid",
    )
    validate.timestamp(existing.get("certified_at"), "existing certificate certified_at")

    # Certification time is metadata, not evidence.  Preserve the existing
    # time while comparing every evidence-bearing byte digest and count to the
    # freshly recomputed contract.
    expected = json.loads(json.dumps(freshly_recomputed, ensure_ascii=False, allow_nan=False))
    expected["certified_at"] = existing["certified_at"]
    expected.pop("manifest_sha256", None)
    expected["manifest_sha256"] = validate.canonical_sha256(expected)
    require(
        existing == expected,
        "existing collection certificate differs from freshly validated artifacts",
    )
    return existing


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--truth", required=True, type=Path)
    parser.add_argument("--freeze", required=True, type=Path)
    parser.add_argument("--calibration-schedule", required=True, type=Path)
    parser.add_argument("--calibration-runs", required=True, type=Path)
    parser.add_argument("--evaluation-schedule", required=True, type=Path)
    parser.add_argument("--execution-lock", required=True, type=Path)
    parser.add_argument("--runs", required=True, type=Path, help="evaluation run directory")
    parser.add_argument("--results", required=True, type=Path, help="candidate analyze.py JSON")
    parser.add_argument("--scored-csv", required=True, type=Path, help="candidate analyze.py CSV")
    publication = parser.add_mutually_exclusive_group(required=True)
    publication.add_argument("--output", type=Path, help="atomically publish a new certification manifest")
    publication.add_argument(
        "--verify-existing", type=Path,
        help="rerun all checks and compare an existing manifest instead of trusting its self-digest",
    )
    parser.add_argument("--analysis-timeout-seconds", type=int, default=1800)
    args = parser.parse_args()

    try:
        if args.output is not None:
            _validate_output_path(
                args.output,
                [
                    args.truth, args.freeze, args.calibration_schedule, args.evaluation_schedule,
                    args.execution_lock, args.results, args.scored_csv,
                ],
                [args.calibration_runs, args.runs],
            )
        certificate = certify_collection(
            truth=args.truth,
            freeze=args.freeze,
            calibration_schedule=args.calibration_schedule,
            calibration_runs=args.calibration_runs,
            evaluation_schedule=args.evaluation_schedule,
            execution_lock=args.execution_lock,
            evaluation_runs=args.runs,
            results=args.results,
            scored_csv=args.scored_csv,
            analysis_timeout_seconds=args.analysis_timeout_seconds,
        )
        if args.verify_existing is not None:
            verified = verify_existing_manifest(args.verify_existing, certificate)
        else:
            write_atomic_manifest(args.output, certificate)
    except (ValueError, KeyError, TypeError, OSError) as error:
        raise SystemExit(f"workflow collection certification failed: {error}") from error
    if args.verify_existing is not None:
        print(
            f"verified existing collection manifest: {args.verify_existing} "
            f"({verified['manifest_sha256']}, 18 + 636 = 654 registered runs)"
        )
    else:
        print(
            f"wrote certified collection manifest: {args.output} "
            f"({certificate['manifest_sha256']}, 18 + 636 = 654 registered runs)"
        )


if __name__ == "__main__":
    main()
