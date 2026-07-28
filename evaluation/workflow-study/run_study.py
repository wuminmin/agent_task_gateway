#!/usr/bin/env python3
"""Build and execute the preregistered isolated workflow-study schedule."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import random
import shlex
import subprocess
import tempfile
from pathlib import Path

import validate


def read_freeze(path: Path, execution_lock: Path, protocol: dict) -> dict:
    frozen = validate.load(path)
    if frozen.get("status") != "frozen_before_agent_runs" or frozen.get("study_id") != protocol["study_id"]:
        raise ValueError("budget freeze is missing or belongs to another study")
    claimed = frozen.get("freeze_sha256", "")
    payload = dict(frozen)
    payload.pop("freeze_sha256", None)
    if claimed != validate.canonical_sha256(payload):
        raise ValueError("budget freeze digest does not match its payload")
    validate.validate_execution_lock(execution_lock, protocol["study_id"])
    if frozen.get("execution_lock_sha256") != hashlib.sha256(execution_lock.read_bytes()).hexdigest():
        raise ValueError("execution lock differs from the frozen artifact")
    if frozen.get("task_manifest_sha256") != hashlib.sha256(validate.TASKS.read_bytes()).hexdigest():
        raise ValueError("task manifest differs from the frozen artifact")
    if frozen.get("protocol_sha256") != hashlib.sha256(validate.PROTOCOL.read_bytes()).hexdigest():
        raise ValueError("protocol differs from the frozen artifact")
    if frozen.get("display_context_sha256") != validate.display_context_sha256():
        raise ValueError("expert display context differs from the frozen artifact")
    return frozen


def scaled_budget(budget: dict[str, int], multiplier: float, arm: str | None = None) -> dict[str, int]:
    scaled = {unit: math.floor(amount * multiplier) for unit, amount in budget.items()}
    if arm == "taskgate_v3":
        scaled = {unit: max(1, amount) for unit, amount in scaled.items()}
    return scaled


def cell_id(study_id: str, task_id: str, arm: str, seed: int, phase: str, multiplier: float) -> str:
    material = f"{study_id}\0{task_id}\0{arm}\0{seed}\0{phase}\0{multiplier:.6f}".encode()
    return "ws-" + hashlib.sha256(material).hexdigest()[:24]


def randomized_arms(task_id: str) -> list[str]:
    arms = sorted(validate.ARMS)
    seed = int.from_bytes(hashlib.sha256(("arm-order\0" + task_id).encode()).digest()[:8], "big")
    random.Random(seed).shuffle(arms)
    return arms


def make_schedule(tasks_doc: dict, protocol: dict, frozen: dict) -> dict:
    sampling = protocol["sampling"]
    cells = []
    for task in sorted(tasks_doc["tasks"], key=lambda item: item["id"]):
        task_id = task["id"]
        base_order = randomized_arms(task_id)
        for seed in range(sampling["agent_seeds_per_primary_arm"]):
            order = base_order[seed:] + base_order[:seed]
            for position, arm in enumerate(order):
                if arm == "unlimited":
                    phase = "unlimited_upper_bound"
                    budget = {}
                else:
                    phase = "primary"
                    budget = frozen["budgets"][task_id][arm]
                run_id = cell_id(protocol["study_id"], task_id, arm, seed, phase, 1.0)
                cells.append(
                    {
                        "run_id": run_id,
                        "task_id": task_id,
                        "arm": arm,
                        "seed": seed,
                        "phase": phase,
                        "budget_multiplier": 1.0,
                        "budget": budget,
                        "sequence_position": position,
                        "isolation_namespace": run_id,
                    }
                )
        pareto = []
        for seed in sampling["pareto_sweep_seeds"]:
            for multiplier in sampling["pareto_budget_multipliers"]:
                for arm in sorted(validate.PRIMARY_ARMS):
                    run_id = cell_id(protocol["study_id"], task_id, arm, seed, "pareto_sweep", float(multiplier))
                    pareto.append(
                        {
                            "run_id": run_id,
                            "task_id": task_id,
                            "arm": arm,
                            "seed": seed,
                            "phase": "pareto_sweep",
                            "budget_multiplier": float(multiplier),
                            "budget": scaled_budget(frozen["budgets"][task_id][arm], float(multiplier), arm),
                            "isolation_namespace": run_id,
                        }
                    )
        seed = int.from_bytes(hashlib.sha256(("pareto-order\0" + task_id).encode()).digest()[:8], "big")
        random.Random(seed).shuffle(pareto)
        for position, cell in enumerate(pareto):
            cell["sequence_position"] = position
            cells.append(cell)
    if len(cells) != sampling["planned_agent_runs"]:
        raise ValueError(f"schedule has {len(cells)} runs, expected {sampling['planned_agent_runs']}")
    payload = {
        "schema_version": 1,
        "study_id": protocol["study_id"],
        "budget_freeze_sha256": frozen["freeze_sha256"],
        "execution_lock_sha256": frozen["execution_lock_sha256"],
        "budget_rejection_envelope": "taskgate-study-budget-rejection-v1",
        "cells": cells,
    }
    payload["schedule_sha256"] = validate.canonical_sha256(payload)
    return payload


def write_atomic(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=path.parent, delete=False) as temporary:
        json.dump(value, temporary, ensure_ascii=False, indent=2, sort_keys=True)
        temporary.write("\n")
        temporary_path = Path(temporary.name)
    temporary_path.replace(path)


def execute(schedule: dict, command: str, output: Path, timeout_seconds: int) -> None:
    argv = shlex.split(command)
    if not argv:
        raise ValueError("agent adapter command is empty")
    for index, cell in enumerate(schedule["cells"], start=1):
        destination = output / f"{cell['run_id']}.json"
        if destination.exists():
            existing = validate.load(destination)
            if existing.get("run_id") != cell["run_id"]:
                raise ValueError(f"existing run file conflicts: {destination}")
            continue
        invocation = {
            **cell,
            "study_id": schedule["study_id"],
            "budget_freeze_sha256": schedule["budget_freeze_sha256"],
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
        completed = subprocess.run(
            argv,
            input=json.dumps(invocation, ensure_ascii=False),
            text=True,
            capture_output=True,
            timeout=timeout_seconds,
            check=False,
            env={**os.environ, "WORKFLOW_STUDY_RUN_ID": cell["run_id"]},
        )
        if completed.returncode != 0:
            raise RuntimeError(f"agent adapter failed for {cell['run_id']}: exit {completed.returncode}")
        try:
            record = json.loads(completed.stdout)
        except json.JSONDecodeError as error:
            raise ValueError(f"agent adapter returned invalid JSON for {cell['run_id']}") from error
        for field in ("run_id", "task_id", "arm", "seed", "phase", "budget_multiplier", "budget"):
            if record.get(field) != cell[field]:
                raise ValueError(f"agent adapter changed registered {field} for {cell['run_id']}")
        for field in ("budget_freeze_sha256", "execution_lock_sha256", "budget_rejection_envelope"):
            if record.get(field) != schedule[field]:
                raise ValueError(f"agent adapter changed locked {field} for {cell['run_id']}")
        write_atomic(destination, record)
        print(f"[{index}/{len(schedule['cells'])}] wrote {destination.name}")


def validate_schedule(schedule: dict, protocol: dict) -> None:
    if schedule.get("study_id") != protocol["study_id"]:
        raise ValueError("schedule belongs to another study")
    if schedule.get("budget_rejection_envelope") != "taskgate-study-budget-rejection-v1":
        raise ValueError("schedule changed the common rejection envelope")
    cells = schedule.get("cells")
    if not isinstance(cells, list) or len(cells) != protocol["sampling"]["planned_agent_runs"]:
        raise ValueError("schedule has incomplete registered coverage")
    run_ids = [cell.get("run_id") for cell in cells]
    namespaces = [cell.get("isolation_namespace") for cell in cells]
    if len(run_ids) != len(set(run_ids)) or len(namespaces) != len(set(namespaces)):
        raise ValueError("schedule reuses a run or isolation namespace")


def main() -> None:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="action", required=True)
    schedule_parser = subparsers.add_parser("schedule")
    schedule_parser.add_argument("--freeze", required=True, type=Path)
    schedule_parser.add_argument("--execution-lock", required=True, type=Path)
    schedule_parser.add_argument("--output", required=True, type=Path)
    run_parser = subparsers.add_parser("run")
    run_parser.add_argument("--schedule", required=True, type=Path)
    run_parser.add_argument("--freeze", required=True, type=Path)
    run_parser.add_argument("--execution-lock", required=True, type=Path)
    run_parser.add_argument("--output", required=True, type=Path)
    run_parser.add_argument("--agent-command", default=os.getenv("WORKFLOW_AGENT_COMMAND", ""))
    run_parser.add_argument("--timeout-seconds", type=int, default=1800)
    args = parser.parse_args()

    tasks_doc, protocol = validate.validate_design()
    if args.action == "schedule":
        frozen = read_freeze(args.freeze, args.execution_lock, protocol)
        schedule = make_schedule(tasks_doc, protocol, frozen)
        write_atomic(args.output, schedule)
        print(f"wrote schedule: {args.output} ({len(schedule['cells'])} isolated runs)")
    else:
        schedule = validate.load(args.schedule)
        claimed = schedule.get("schedule_sha256", "")
        payload = dict(schedule)
        payload.pop("schedule_sha256", None)
        if claimed != validate.canonical_sha256(payload):
            raise SystemExit("schedule digest mismatch")
        validate_schedule(schedule, protocol)
        frozen = read_freeze(args.freeze, args.execution_lock, protocol)
        if schedule.get("budget_freeze_sha256") != frozen["freeze_sha256"]:
            raise SystemExit("schedule and budget freeze differ")
        if schedule.get("execution_lock_sha256") != frozen["execution_lock_sha256"]:
            raise SystemExit("schedule and execution lock differ")
        execute(schedule, args.agent_command, args.output, args.timeout_seconds)


if __name__ == "__main__":
    main()
