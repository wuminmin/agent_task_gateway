#!/usr/bin/env python3
"""Build and execute calibration or evaluation schedules for the benchmark."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import random
import re
import shlex
import subprocess
import sys
import tempfile
from pathlib import Path

import validate


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
COMPOSE_FILES = (ROOT / "compose.yaml", HERE / "compose.yaml")


def cell_id(study_id: str, schedule_kind: str, task_id: str, arm: str, replicate: int, level: float) -> str:
    return validate.registered_run_id(study_id, schedule_kind, task_id, arm, replicate, level)


def _shuffle(cells: list[dict], label: str) -> None:
    order_key = int.from_bytes(hashlib.sha256(("workflow-order\0" + label).encode()).digest()[:8], "big")
    random.Random(order_key).shuffle(cells)
    for position, cell in enumerate(cells):
        cell["sequence_position"] = position


def _schedule_payload(
    *,
    protocol: dict,
    schedule_kind: str,
    task_manifest: Path,
    freeze_sha: str,
    execution_lock_sha: str,
    cells: list[dict],
) -> dict:
    payload = {
        "schema_version": 2,
        "study_id": protocol["study_id"],
        "schedule_kind": schedule_kind,
        "task_manifest_sha256": validate.file_sha256(task_manifest),
        "protocol_sha256": validate.file_sha256(validate.PROTOCOL),
        "source_file_sha256": validate.source_file_digests(include_generated_truth=False),
        "algorithmic_budget_freeze_sha256": freeze_sha,
        "execution_lock_sha256": execution_lock_sha,
        "budget_rejection_envelope": "taskgate-study-budget-rejection-v1",
        "cells": cells,
    }
    payload["schedule_sha256"] = validate.canonical_sha256(payload)
    return payload


def make_calibration_schedule(calibration_doc: dict, protocol: dict, execution_lock_sha: str) -> dict:
    plan = validate.sampling_plan(protocol)
    cells = []
    for task in calibration_doc["tasks"]:
        for replicate in range(plan["calibration_replicates"]):
            run_id = cell_id(protocol["study_id"], "calibration", task["id"], "unlimited", replicate, 0)
            cells.append(
                {
                    "run_id": run_id,
                    "task_id": task["id"],
                    "domain": task["domain"],
                    "arm": "unlimited",
                    "replicate": replicate,
                    "phase": "algorithmic_calibration",
                    "budget_level": 0,
                    "budget": {},
                    "isolation_namespace": run_id,
                }
            )
    _shuffle(cells, "calibration")
    if len(cells) != plan["calibration_runs"]:
        raise ValueError(f"calibration schedule has {len(cells)} rather than {plan['calibration_runs']} cells")
    return _schedule_payload(
        protocol=protocol,
        schedule_kind="calibration",
        task_manifest=validate.CALIBRATION_TASKS,
        freeze_sha=validate.ZERO_SHA256,
        execution_lock_sha=execution_lock_sha,
        cells=cells,
    )


def make_evaluation_schedule(tasks_doc: dict, protocol: dict, frozen: dict) -> dict:
    plan = validate.sampling_plan(protocol)
    cells = []
    for task in tasks_doc["tasks"]:
        domain = task["domain"]
        for level in validate.BUDGET_LEVELS:
            budgets = frozen["domains"][domain]["levels"][validate.level_key(level)]
            for replicate in range(plan["evaluation_replicates_per_level"]):
                for arm in sorted(validate.PRIMARY_ARMS):
                    run_id = cell_id(protocol["study_id"], "evaluation", task["id"], arm, replicate, level)
                    cells.append(
                        {
                            "run_id": run_id,
                            "task_id": task["id"],
                            "domain": domain,
                            "arm": arm,
                            "replicate": replicate,
                            "phase": "budget_level",
                            "budget_level": level,
                            "budget": budgets[arm],
                            "isolation_namespace": run_id,
                        }
                    )
        for replicate in range(plan["unlimited_replicates"]):
            run_id = cell_id(protocol["study_id"], "evaluation", task["id"], "unlimited", replicate, 0)
            cells.append(
                {
                    "run_id": run_id,
                    "task_id": task["id"],
                    "domain": domain,
                    "arm": "unlimited",
                    "replicate": replicate,
                    "phase": "unbudgeted_reference",
                    "budget_level": 0,
                    "budget": {},
                    "isolation_namespace": run_id,
                }
            )
    _shuffle(cells, "evaluation")
    if len(cells) != plan["planned_evaluation_runs"]:
        raise ValueError(f"evaluation schedule has {len(cells)} rather than {plan['planned_evaluation_runs']} cells")
    return _schedule_payload(
        protocol=protocol,
        schedule_kind="evaluation",
        task_manifest=validate.TASKS,
        freeze_sha=frozen["freeze_sha256"],
        execution_lock_sha=frozen["execution_lock_sha256"],
        cells=cells,
    )


def write_atomic(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=path.parent, delete=False) as temporary:
        json.dump(value, temporary, ensure_ascii=False, indent=2, sort_keys=True)
        temporary.write("\n")
        temporary_path = Path(temporary.name)
    temporary_path.replace(path)


def _validate_cell(cell: dict, tasks: dict[str, dict], schedule: dict, protocol: dict, frozen: dict | None) -> tuple:
    task_id = cell.get("task_id")
    if task_id not in tasks:
        raise ValueError(f"schedule contains unknown task {task_id!r}")
    if cell.get("domain") != tasks[task_id]["domain"]:
        raise ValueError(f"schedule domain differs for {task_id}")
    replicate = cell.get("replicate")
    if not isinstance(replicate, int) or isinstance(replicate, bool) or replicate < 0:
        raise ValueError(f"schedule has invalid replicate for {task_id}")
    arm = cell.get("arm")
    phase = cell.get("phase")
    level = float(cell.get("budget_level", -1))
    expected_id = cell_id(protocol["study_id"], schedule["schedule_kind"], task_id, arm, replicate, level)
    if cell.get("run_id") != expected_id or cell.get("isolation_namespace") != expected_id:
        raise ValueError(f"schedule cell identity differs for {task_id}/{arm}/{replicate}/{level}")
    if schedule["schedule_kind"] == "calibration":
        if arm != "unlimited" or phase != "algorithmic_calibration" or level != 0 or cell.get("budget") != {}:
            raise ValueError(f"invalid calibration schedule cell {expected_id}")
    elif arm == "unlimited":
        if phase != "unbudgeted_reference" or level != 0 or cell.get("budget") != {}:
            raise ValueError(f"invalid unlimited schedule cell {expected_id}")
    else:
        if arm not in validate.PRIMARY_ARMS or phase != "budget_level" or level not in validate.BUDGET_LEVELS:
            raise ValueError(f"invalid budget-level schedule cell {expected_id}")
        assert frozen is not None
        expected_budget = frozen["domains"][tasks[task_id]["domain"]]["levels"][validate.level_key(level)][arm]
        if cell.get("budget") != expected_budget:
            raise ValueError(f"schedule changed the frozen budget for {expected_id}")
    return task_id, arm, replicate, phase, level


def validate_schedule(
    schedule: dict,
    tasks_doc: dict,
    protocol: dict,
    execution_lock_sha: str,
    frozen: dict | None = None,
) -> None:
    claimed = schedule.get("schedule_sha256", "")
    payload = dict(schedule)
    payload.pop("schedule_sha256", None)
    if claimed != validate.canonical_sha256(payload):
        raise ValueError("schedule digest mismatch")
    kind = schedule.get("schedule_kind")
    if schedule.get("schema_version") != 2 or kind not in {"calibration", "evaluation"}:
        raise ValueError("unsupported schedule identity")
    if schedule.get("study_id") != protocol["study_id"]:
        raise ValueError("schedule belongs to another study")
    if schedule.get("protocol_sha256") != validate.file_sha256(validate.PROTOCOL):
        raise ValueError("protocol differs from schedule")
    if schedule.get("source_file_sha256") != validate.source_file_digests(include_generated_truth=False):
        raise ValueError("database/oracle/risk sources differ from schedule")
    if schedule.get("execution_lock_sha256") != execution_lock_sha:
        raise ValueError("execution lock differs from schedule")
    if schedule.get("budget_rejection_envelope") != "taskgate-study-budget-rejection-v1":
        raise ValueError("schedule changed the common rejection envelope")
    expected_manifest = validate.CALIBRATION_TASKS if kind == "calibration" else validate.TASKS
    if schedule.get("task_manifest_sha256") != validate.file_sha256(expected_manifest):
        raise ValueError("task manifest differs from schedule")
    if kind == "calibration":
        if frozen is not None or schedule.get("algorithmic_budget_freeze_sha256") != validate.ZERO_SHA256:
            raise ValueError("calibration schedule must precede the algorithmic freeze")
    else:
        if frozen is None or schedule.get("algorithmic_budget_freeze_sha256") != frozen["freeze_sha256"]:
            raise ValueError("evaluation schedule differs from algorithmic freeze")
    tasks = {task["id"]: task for task in tasks_doc["tasks"]}
    cells = schedule.get("cells")
    if not isinstance(cells, list):
        raise ValueError("schedule cells are missing")
    observed = [_validate_cell(cell, tasks, schedule, protocol, frozen) for cell in cells]
    if len(observed) != len(set(observed)):
        raise ValueError("schedule repeats a registered cell")
    plan = validate.sampling_plan(protocol)
    if kind == "calibration":
        expected = {
            (task_id, "unlimited", replicate, "algorithmic_calibration", 0.0)
            for task_id in tasks
            for replicate in range(plan["calibration_replicates"])
        }
    else:
        expected = {
            (task_id, arm, replicate, "budget_level", level)
            for task_id in tasks
            for arm in validate.PRIMARY_ARMS
            for level in validate.BUDGET_LEVELS
            for replicate in range(plan["evaluation_replicates_per_level"])
        }
        expected.update(
            (task_id, "unlimited", replicate, "unbudgeted_reference", 0.0)
            for task_id in tasks
            for replicate in range(plan["unlimited_replicates"])
        )
    if set(observed) != expected:
        raise ValueError(f"schedule coverage differs: {len(expected - set(observed))} missing, {len(set(observed) - expected)} extra")
    expected_count = plan["calibration_runs" if kind == "calibration" else "planned_evaluation_runs"]
    if len(observed) != expected_count:
        raise ValueError(f"schedule has {len(observed)} rather than {expected_count} cells")
    positions = [cell.get("sequence_position") for cell in cells]
    if positions != list(range(expected_count)):
        raise ValueError("schedule list order differs from its sequence positions")
    registered = (
        make_calibration_schedule(tasks_doc, protocol, execution_lock_sha)
        if kind == "calibration"
        else make_evaluation_schedule(tasks_doc, protocol, frozen)
    )
    if [cell["run_id"] for cell in cells] != [cell["run_id"] for cell in registered["cells"]]:
        raise ValueError("schedule order differs from the deterministic registered shuffle")


def compose_project(run_id: str) -> str:
    project = re.sub(r"[^a-z0-9-]", "-", run_id.lower())[:55]
    if not project or project != run_id.lower()[:55]:
        raise ValueError("run id cannot be mapped safely to an exact Compose project")
    return project


def cleanup_project(run_id: str) -> None:
    project = compose_project(run_id)
    command = [
        "docker", "compose", "-p", project,
        "-f", str(COMPOSE_FILES[0]), "-f", str(COMPOSE_FILES[1]),
        "down", "-v", "--remove-orphans", "--rmi", "local",
    ]
    try:
        completed = subprocess.run(
            command, cwd=ROOT, env=os.environ, text=True,
            capture_output=True, timeout=180, check=False,
        )
    except subprocess.TimeoutExpired as error:
        raise ValueError(f"exact Compose cleanup timed out for {project}") from error
    if completed.returncode != 0:
        raise ValueError(f"exact Compose cleanup failed for {project} (exit {completed.returncode})")


def _validate_external_record(record: object, cell: dict, schedule: dict) -> dict:
    if not isinstance(record, dict) or record.get("schema_version") != 3:
        raise ValueError("adapter output is not a schema-v3 run record")
    cell_fields = ("run_id", "task_id", "domain", "arm", "replicate", "phase", "budget_level", "budget")
    locked_fields = ("algorithmic_budget_freeze_sha256", "execution_lock_sha256", "budget_rejection_envelope")
    if any(record.get(field) != cell[field] for field in cell_fields) or any(
        record.get(field) != schedule[field] for field in locked_fields
    ):
        raise ValueError("adapter output violates the registered cell contract")
    facts = record.get("fact_evidence")
    budget_audit = record.get("gateway_budget_audit")
    if not isinstance(facts, list) or record.get("fact_evidence_sha256") != validate.canonical_sha256(facts):
        raise ValueError("adapter output lacks verifiable Fact evidence")
    if not isinstance(budget_audit, dict) or record.get("gateway_budget_audit_sha256") != validate.canonical_sha256(budget_audit):
        raise ValueError("adapter output lacks a verifiable post-run budget audit")
    return record


def execute(schedule: dict, command: str, output: Path, timeout_seconds: int, lock: dict) -> None:
    argv = shlex.split(command)
    if not argv:
        raise ValueError("agent adapter command is empty")
    if schedule.get("source_file_sha256") != validate.source_file_digests(include_generated_truth=False):
        raise ValueError("database/oracle/risk sources drifted before schedule execution")
    for index, cell in enumerate(schedule["cells"], start=1):
        destination = output / f"{cell['run_id']}.json"
        if destination.exists():
            existing = validate.load(destination)
            if any(existing.get(field) != cell[field] for field in ("run_id", "task_id", "domain", "arm", "replicate", "phase", "budget_level", "budget")):
                raise ValueError(f"existing run file conflicts: {destination}")
            if any(
                existing.get(field) != schedule[field]
                for field in ("algorithmic_budget_freeze_sha256", "execution_lock_sha256", "budget_rejection_envelope")
            ):
                raise ValueError(f"existing run file uses different locked artifacts: {destination}")
            continue
        invocation = {
            **cell,
            "study_id": schedule["study_id"],
            "algorithmic_budget_freeze_sha256": schedule["algorithmic_budget_freeze_sha256"],
            "execution_lock_sha256": schedule["execution_lock_sha256"],
            "budget_rejection_envelope": schedule["budget_rejection_envelope"],
            "requirements": {
                "fresh_database_instance": True,
                "fresh_root_task": True,
                "fresh_cache_namespace": True,
                "fresh_conversation": True,
                "v3_audit_only_for_non_taskgate": True,
            },
        }
        try:
            completed = subprocess.run(
                argv,
                input=json.dumps(invocation, ensure_ascii=False),
                text=True,
                capture_output=True,
                timeout=timeout_seconds,
                check=False,
                env={**os.environ, "WORKFLOW_STUDY_RUN_ID": cell["run_id"]},
            )
        except subprocess.TimeoutExpired:
            cleanup_project(cell["run_id"])
            raise ValueError(
                f"adapter timed out for {cell['run_id']}; no zero-exposure record was fabricated and the campaign must fail"
            )
        if completed.returncode != 0:
            cleanup_project(cell["run_id"])
            raise ValueError(
                f"adapter exited {completed.returncode} for {cell['run_id']}; no trustworthy run record exists"
            )
        try:
            parsed = json.loads(completed.stdout)
        except json.JSONDecodeError as error:
            cleanup_project(cell["run_id"])
            raise ValueError(f"adapter emitted invalid JSON for {cell['run_id']}; campaign must fail") from error
        try:
            record = _validate_external_record(parsed, cell, schedule)
        except ValueError:
            cleanup_project(cell["run_id"])
            raise
        if os.getenv("WORKFLOW_STUDY_KEEP_STACK") != "1":
            cleanup_project(cell["run_id"])
        write_atomic(destination, record)
        print(f"[{index}/{len(schedule['cells'])}] wrote {destination.name} ({record.get('status')})")


def main() -> None:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="action", required=True)
    calibration_parser = subparsers.add_parser("calibration-schedule")
    calibration_parser.add_argument("--execution-lock", required=True, type=Path)
    calibration_parser.add_argument("--output", required=True, type=Path)
    evaluation_parser = subparsers.add_parser("evaluation-schedule")
    evaluation_parser.add_argument("--freeze", required=True, type=Path)
    evaluation_parser.add_argument("--calibration-runs", required=True, type=Path)
    evaluation_parser.add_argument("--execution-lock", required=True, type=Path)
    evaluation_parser.add_argument("--output", required=True, type=Path)
    run_parser = subparsers.add_parser("run")
    run_parser.add_argument("--schedule", required=True, type=Path)
    run_parser.add_argument("--freeze", type=Path)
    run_parser.add_argument("--calibration-runs", type=Path)
    run_parser.add_argument("--execution-lock", required=True, type=Path)
    run_parser.add_argument("--output", required=True, type=Path)
    run_parser.add_argument("--timeout-seconds", type=int, default=1800)
    args = parser.parse_args()

    try:
        tasks_doc, calibration_doc, protocol = validate.validate_design()
        lock = validate.validate_execution_lock(args.execution_lock, protocol["study_id"])
        lock_sha = validate.file_sha256(args.execution_lock)
        if args.action == "calibration-schedule":
            schedule = make_calibration_schedule(calibration_doc, protocol, lock_sha)
            write_atomic(args.output, schedule)
            print(f"wrote calibration schedule: {args.output} ({len(schedule['cells'])} isolated runs)")
            return
        if args.action == "evaluation-schedule":
            frozen = validate.validate_algorithmic_freeze(
                args.freeze, protocol, args.execution_lock, args.calibration_runs,
            )
            schedule = make_evaluation_schedule(tasks_doc, protocol, frozen)
            write_atomic(args.output, schedule)
            print(f"wrote evaluation schedule: {args.output} ({len(schedule['cells'])} isolated runs)")
            return
        schedule = validate.load(args.schedule)
        kind = schedule.get("schedule_kind")
        if kind == "calibration":
            if args.freeze is not None:
                raise ValueError("calibration execution must not receive a budget freeze")
            frozen = None
            manifest = calibration_doc
        elif kind == "evaluation":
            if args.freeze is None:
                raise ValueError("evaluation execution requires --freeze")
            if args.calibration_runs is None:
                raise ValueError("evaluation execution requires --calibration-runs")
            frozen = validate.validate_algorithmic_freeze(
                args.freeze, protocol, args.execution_lock, args.calibration_runs,
            )
            manifest = tasks_doc
        else:
            raise ValueError("unknown schedule kind")
        validate_schedule(schedule, manifest, protocol, lock_sha, frozen)
        adapter_command = f"{shlex.quote(sys.executable)} {shlex.quote(str(validate.HERE / 'deepseek_agent_adapter.py'))}"
        execute(schedule, adapter_command, args.output, args.timeout_seconds, lock)
    except ValueError as error:
        raise SystemExit(f"workflow runner failed: {error}") from error


if __name__ == "__main__":
    main()
