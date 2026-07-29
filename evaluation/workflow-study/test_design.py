#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import analyze
import freeze_budgets
import run_study
import validate


def provider_lock_fields() -> dict:
    return {
        "provider": "deepseek",
        "model": "deepseek-v4-flash",
        "model_version": "DeepSeek-V4-Flash",
        "thinking_mode": "disabled",
        "temperature": 0,
        "top_p": 1.0,
        "max_tokens": 4096,
        "request_timeout_seconds": 300,
        "adapter_timeout_seconds": 1800,
        "max_tool_turns": 16,
        "api_retry": {
            "max_attempts": 5,
            "initial_backoff_seconds": 2.0,
            "max_backoff_seconds": 30.0,
            "retryable_http_statuses": [429, 500, 502, 503, 504],
            "retry_insufficient_system_resource": True,
        },
        "infrastructure_retry": {
            "compose_start_max_attempts": 3,
            "compose_start_backoff_seconds": 2.0,
        },
        "pricing_usd_per_million_tokens": {
            "prompt_cache_hit": 0.0028,
            "prompt_cache_miss": 0.14,
            "completion": 0.28,
        },
        "pricing_source": "https://api-docs.deepseek.com/quick_start/pricing/",
        "phase_cost_limits_usd": {"calibration": 2.0, "evaluation": 18.0},
        "container_images": {
            name: {
                "requested_reference": f"workflow-test-{name}:v1",
                "image_id": "sha256:" + character * 64,
                "repo_digests": [],
            }
            for name, character in (("gateway", "1"), ("oa_demo", "2"), ("postgres", "3"))
        },
        "container_runtime": {
            "docker_server_version": "29.1.3",
            "docker_compose_version": "2.40.3",
        },
        "api_base_url": "https://api.deepseek.com",
    }


def scoring_run(task: dict, answer: dict, queries: list[dict], *, status: str = "completed") -> dict:
    run = {
        "run_id": f"score-{task['id']}",
        "task_id": task["id"],
        "domain": task["domain"],
        "arm": "query_count",
        "replicate": 0,
        "phase": "budget_level",
        "budget_level": 0.5,
        "status": status,
        "queries": queries,
        "final_answer": answer,
        "final_answer_text": "",
        "runtime_budget_rejections": 0,
        "final_format_repair_attempts": 0,
        "compose_start_attempts": 1,
        "neutral_disclosure": {
            **{field: 0 for field in validate.NEUTRAL_DISCLOSURE_FIELDS},
            "disclosed_negative_propositions": 1,
        },
    }
    if status != "completed":
        run["failure"] = {"category": "adapter_nonzero_exit"}
    return run


def valid_calibration_record(
    task: dict,
    protocol: dict,
    lock: dict,
    lock_sha: str,
    replicate: int = 0,
) -> dict:
    """Construct a minimal completed trace whose usage and risk are evidence-derived."""
    run_id = validate.registered_run_id(
        protocol["study_id"], "calibration", task["id"], "unlimited", replicate, 0,
    )
    query_id = f"query-{run_id}"
    visible = {"columns": [], "rows": [], "row_count": 0, "limited": False}
    response = validate.canonical_json_bytes(visible)
    identity = {
        "profile": "taskgate-exposure-v3",
        "kind": "outcome",
        "query_normal_form_version": "taskgate-query-normal-form-v1",
        "query_normal_form_sha256": validate.canonical_sha256({"query_id": query_id}),
        "outcome_sha256": validate.canonical_sha256({"visible": visible}),
        # The Go identity omits the zero-valued outcome_rows field.
    }
    facts = [{
        "ledger_kind": "OUTCOME",
        "fact_sha256": validate.canonical_sha256(identity),
        "identity": identity,
        "query_ids": [query_id],
    }]
    measured = validate.study_risk.measure(facts, task)
    common_risk = {field: measured[field] for field in validate.COMMON_RISK_FIELDS}
    neutral = {field: measured[field] for field in validate.NEUTRAL_DISCLOSURE_FIELDS}
    native_usage = {
        "successful_queries": 1,
        "returned_rows": 0,
        "serialized_bytes": len(response),
    }
    root_task_id = f"root-{run_id}"
    gateway_audit = {
        "available": True,
        "snapshot": {
            "task_id": root_task_id,
            "budget": {
                "limits": {"queries": 100, "rows": 100000, "db_ms": 120000},
                "used": {"queries": 1, "rows": 0, "db_ms": 0},
                "reserved": {"queries": 0, "rows": 0, "db_ms": 0},
            },
            "exposure_budget": {
                "limits": {"release_facts": 1000000, "influence_facts": 10000000, "outcome_facts": 1000},
                "used": {
                    field: common_risk[field]
                    for field in ("release_facts", "influence_facts", "outcome_facts")
                },
            },
        },
    }
    return {
        "schema_version": 3,
        "study_id": protocol["study_id"],
        "run_id": run_id,
        "task_id": task["id"],
        "domain": task["domain"],
        "arm": "unlimited",
        "replicate": replicate,
        "phase": "algorithmic_calibration",
        "budget_level": 0,
        "budget": {},
        "model": {
            "provider": lock["provider"],
            "model": lock["model"],
            "version": lock["model_version"],
            "thinking_mode": lock["thinking_mode"],
            "temperature": lock["temperature"],
            "top_p": lock["top_p"],
            "max_tokens": lock["max_tokens"],
            "api_base_url": lock["api_base_url"],
        },
        "provider_response_models": [lock["model"]],
        "provider_api": {
            "model_turns": 1,
            "request_attempts": 1,
            "successful_responses": 1,
            "retry_attempts": 0,
            "token_usage": {
                "prompt_tokens": 2,
                "prompt_cache_hit_tokens": 0,
                "prompt_cache_miss_tokens": 2,
                "completion_tokens": 1,
                "reasoning_tokens": 0,
                "total_tokens": 3,
            },
            "system_fingerprints": ["fp_unit_test"],
            "finish_reasons": ["stop"],
            "estimated_cost_usd": 0.00000056,
        },
        "database_snapshot": "workflow-study-2026-v1",
        "database_instance_id": f"db-{run_id}",
        "root_task_id": root_task_id,
        "cache_namespace": run_id,
        "algorithmic_budget_freeze_sha256": validate.ZERO_SHA256,
        "execution_lock_sha256": lock_sha,
        "budget_rejection_envelope": "taskgate-study-budget-rejection-v1",
        "started_at": "2026-07-28T00:00:00Z",
        "finished_at": "2026-07-28T00:01:00Z",
        "status": "completed",
        "queries": [{
            "request_id": f"request-{run_id}",
            "query_id": query_id,
            "admitted": True,
            "row_count": 0,
            "serialized_bytes": len(response),
            "admitted_response_canonical": response.decode("utf-8"),
            "admitted_response_sha256": hashlib.sha256(response).hexdigest(),
        }],
        "final_answer": {field: "observed" for field in task["required_answer_fields"]},
        "final_answer_text": "observed",
        "fact_evidence": facts,
        "fact_evidence_sha256": validate.canonical_sha256(facts),
        "gateway_budget_audit": gateway_audit,
        "gateway_budget_audit_sha256": validate.canonical_sha256(gateway_audit),
        "common_v3_risk": common_risk,
        "neutral_disclosure": neutral,
        "native_usage": native_usage,
        "runtime_budget_rejections": 0,
        "final_format_repair_attempts": 0,
        "compose_start_attempts": 1,
        "performance": {field: 0 for field in validate.PERFORMANCE_FIELDS},
    }


def synthetic_scored_rows() -> list[dict]:
    task_domains = [
        *( (f"FIN-T{index}", "finance") for index in range(4) ),
        *( (f"SUP-T{index}", "customer_operations") for index in range(4) ),
        *( (f"PROC-T{index}", "risk_compliance") for index in range(4) ),
    ]
    arm_offset = {"taskgate_v3": 0.0, "query_count": 5.0, "returned_rows": 10.0, "serialized_bytes": 15.0}
    rows: list[dict] = []
    for task_index, (task_id, domain) in enumerate(task_domains):
        for arm in analyze.PRIMARY_ARMS:
            for level in analyze.BUDGET_LEVELS:
                for replicate in range(3):
                    quality = min(100.0, 25.0 + 65.0 * level + (3.0 if arm == "taskgate_v3" else 0.0))
                    exposure = 20.0 + 80.0 * level + arm_offset[arm] + task_index
                    row = {field: 0.0 for field in analyze.SUMMARY_FIELDS}
                    row.update({
                        "run_id": f"{task_id}-{arm}-{level}-{replicate}",
                        "task_id": task_id,
                        "domain": domain,
                        "difficulty": "medium",
                        "arm": arm,
                        "replicate": replicate,
                        "phase": "budget_level",
                        "budget_level": level,
                        "status": "completed",
                        "answer_score": quality,
                        "answer_task_complete": float(quality >= 80),
                        "workflow_rubric_score": quality,
                        "workflow_task_complete": float(quality >= 80),
                        "trace_guard_score": 100.0,
                        "answer_evidence_column_coverage": 1.0,
                        "answer_evidence_eligible": 1.0,
                        "numeric_tolerance_normalized_error": 0.0,
                        "numeric_answer_coverage": 1.0,
                        "list_value_precision": 1.0,
                        "list_value_recall": 1.0,
                        "list_value_f1": 1.0,
                        "final_task_failure": 0.0,
                        analyze.EXPOSURE_FIELD: exposure,
                    })
                    for field in analyze.NEUTRAL_FIELDS:
                        row[field] = exposure
                    rows.append(row)
        for replicate in range(5):
            row = {field: 0.0 for field in analyze.SUMMARY_FIELDS}
            row.update({
                "run_id": f"{task_id}-unlimited-{replicate}",
                "task_id": task_id,
                "domain": domain,
                "difficulty": "medium",
                "arm": "unlimited",
                "replicate": replicate,
                "phase": "unbudgeted_reference",
                "budget_level": 0.0,
                "status": "completed",
                "answer_score": 95.0,
                "answer_task_complete": 1.0,
                "workflow_rubric_score": 95.0,
                "workflow_task_complete": 1.0,
                "trace_guard_score": 100.0,
                "answer_evidence_column_coverage": 1.0,
                "answer_evidence_eligible": 1.0,
                "numeric_tolerance_normalized_error": 0.0,
                "numeric_answer_coverage": 1.0,
                "list_value_precision": 1.0,
                "list_value_recall": 1.0,
                "list_value_f1": 1.0,
                "final_task_failure": 0.0,
                analyze.EXPOSURE_FIELD: 125.0 + task_index,
            })
            for field in analyze.NEUTRAL_FIELDS:
                row[field] = 125.0 + task_index
            rows.append(row)
    return rows


def frozen_fixture(tasks_doc: dict) -> dict:
    domains = {}
    base = {
        "taskgate_v3": {"release_facts": 20, "influence_facts": 40, "outcome_facts": 4},
        "query_count": {"successful_queries": 8},
        "returned_rows": {"returned_rows": 80},
        "serialized_bytes": {"serialized_bytes": 8000},
    }
    for domain in validate.DOMAINS:
        domains[domain] = {
            "completed_calibration_runs": 6,
            "base": base,
            "levels": {
                validate.level_key(level): {
                    arm: {unit: max(1, int(amount * level)) for unit, amount in budget.items()}
                    for arm, budget in base.items()
                }
                for level in validate.BUDGET_LEVELS
            },
        }
    return {"freeze_sha256": "a" * 64, "execution_lock_sha256": "b" * 64, "domains": domains}


class ParticipantFreeBenchmarkTest(unittest.TestCase):
    def test_registered_design_is_disjoint_automatic_and_uncollected(self) -> None:
        tasks, calibration, protocol = validate.validate_design()
        self.assertEqual(len(tasks["tasks"]), 12)
        self.assertEqual(len(calibration["tasks"]), 6)
        self.assertTrue(
            {task["id"] for task in tasks["tasks"]}.isdisjoint({task["id"] for task in calibration["tasks"]})
        )
        self.assertEqual(protocol["sampling"]["calibration_runs"], 18)
        self.assertEqual(protocol["sampling"]["planned_evaluation_runs"], 636)
        self.assertEqual(protocol["sampling"]["total_agent_runs"], 654)
        self.assertEqual(protocol["status"], "designed_not_collected")
        self.assertTrue(
            all(item["method"] in validate.AUTOMATIC_METHODS for task in tasks["tasks"] for item in task["rubric"])
        )

    def test_lower_median_is_deterministic_for_six_traces(self) -> None:
        self.assertEqual(freeze_budgets.lower_median([10, 4, 8, 2, 6, 12]), 6)
        self.assertEqual(freeze_budgets.lower_median([9, 3, 6]), 6)
        with self.assertRaisesRegex(ValueError, "empty"):
            freeze_budgets.lower_median([])

    def test_freeze_aggregates_six_traces_per_domain_and_builds_four_levels(self) -> None:
        _, calibration, _ = validate.validate_design()
        domain_position = {domain: 0 for domain in validate.DOMAINS}
        records = []
        for task in calibration["tasks"]:
            for _ in range(3):
                domain_position[task["domain"]] += 1
                value = domain_position[task["domain"]]
                records.append(
                    {
                        "task_id": task["id"],
                        "common_v3_risk": {
                            "release_facts": value,
                            "influence_facts": value * 2,
                            "outcome_facts": value,
                        },
                        "native_usage": {
                            "successful_queries": value,
                            "returned_rows": value * 10,
                            "serialized_bytes": value * 100,
                        },
                    }
                )
        domains = freeze_budgets.aggregate(records, calibration)
        self.assertEqual(domains["finance"]["base"]["query_count"], {"successful_queries": 3})
        self.assertEqual(
            domains["finance"]["levels"]["0.25"]["taskgate_v3"],
            {"influence_facts": 1, "outcome_facts": 1, "release_facts": 1},
        )
        self.assertEqual(set(domains["finance"]["levels"]), {"0.25", "0.50", "0.75", "1.00"})

    def test_freeze_binds_completed_calibration_records_and_source_files(self) -> None:
        tasks, calibration, protocol = validate.validate_design()
        answer_contract = {
            task["id"]: sorted({item["answer_path"].split(".")[0] for item in task["rubric"] if item.get("answer_path")})
            for task in tasks["tasks"]
        }
        answer_contract.update({task["id"]: sorted(task["required_answer_fields"]) for task in calibration["tasks"]})
        lock = {
            "schema_version": 2,
            "study_id": protocol["study_id"],
            "campaign_id": "unit-test-campaign",
            "locked_at": "2026-07-27T23:59:00Z",
            **provider_lock_fields(),
            "system_prompt_sha256": validate.file_sha256(validate.HERE / "system-prompt.txt"),
            "tool_surface_sha256": validate.file_sha256(validate.HERE / "agent-tool-surface.json"),
            "agent_adapter_sha256": validate.file_sha256(validate.HERE / "deepseek_agent_adapter.py"),
            "answer_schema_sha256": validate.canonical_sha256(answer_contract),
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            lock_path = root / "execution-lock.json"
            lock_path.write_text(json.dumps(lock), encoding="utf-8")
            lock_sha = validate.file_sha256(lock_path)
            runs = root / "calibration-runs"
            runs.mkdir()
            for task in calibration["tasks"]:
                for replicate in range(3):
                    run_id = validate.registered_run_id(
                        protocol["study_id"], "calibration", task["id"], "unlimited", replicate, 0,
                    )
                    record = valid_calibration_record(task, protocol, lock, lock_sha, replicate)
                    (runs / f"{run_id}.json").write_text(json.dumps(record), encoding="utf-8")
            frozen = freeze_budgets.build_freeze(runs, lock_path, "2026-07-28T00:02:00Z")
            freeze_path = root / "freeze.json"
            freeze_path.write_text(json.dumps(frozen), encoding="utf-8")
            validated = validate.validate_algorithmic_freeze(freeze_path, protocol, lock_path, runs)
        self.assertEqual(validated["source_file_sha256"], validate.source_file_digests())
        self.assertEqual(len(validated["calibration_run_records"]), 18)

    def test_offline_record_validation_rejects_identity_model_answer_and_run_id_tampering(self) -> None:
        _, calibration, protocol = validate.validate_design()
        task = calibration["tasks"][0]
        lock = {
            "locked_at": "2026-07-27T23:59:00Z",
            **provider_lock_fields(),
        }
        lock_sha = "b" * 64
        record = valid_calibration_record(task, protocol, lock, lock_sha)
        validate._validate_run_record(
            record, Path("valid.json"), task, protocol, lock_sha, lock, "calibration",
        )

        mutations = [
            ("study identity", lambda value: value.update(study_id="another-study")),
            ("model configuration", lambda value: value["model"].update(version="other")),
            ("field contract", lambda value: value["final_answer"].update(extra="not registered")),
            ("run identity", lambda value: value.update(run_id="ws-not-the-registered-cell")),
        ]
        for message, mutate in mutations:
            tampered = json.loads(json.dumps(record))
            mutate(tampered)
            with self.subTest(message=message), self.assertRaisesRegex(ValueError, message):
                validate._validate_run_record(
                    tampered, Path("tampered.json"), task, protocol, lock_sha, lock, "calibration",
                )

    def test_offline_record_validation_recomputes_response_fact_rejection_and_budget_evidence(self) -> None:
        _, calibration, protocol = validate.validate_design()
        task = calibration["tasks"][0]
        lock = {
            "locked_at": "2026-07-27T23:59:00Z", **provider_lock_fields(),
        }
        lock_sha = "b" * 64
        record = valid_calibration_record(task, protocol, lock, lock_sha)

        def alter_fact(value: dict) -> None:
            value["fact_evidence"][0]["identity"]["outcome_rows"] = 1
            value["fact_evidence_sha256"] = validate.canonical_sha256(value["fact_evidence"])

        def alter_audit(value: dict) -> None:
            value["gateway_budget_audit"]["snapshot"]["budget"]["limits"]["rows"] = 99999
            value["gateway_budget_audit_sha256"] = validate.canonical_sha256(value["gateway_budget_audit"])

        mutations = [
            ("admitted response hash mismatch", lambda value: value["queries"][0].update(admitted_response_canonical="{}")),
            ("native_usage is not response-derived", lambda value: value["native_usage"].update(serialized_bytes=0)),
            ("runtime rejection count is not trace-derived", lambda value: value.update(runtime_budget_rejections=1)),
            ("neutral_disclosure is not evidence-derived", alter_fact),
            ("gateway native limits differ", alter_audit),
        ]
        for message, mutate in mutations:
            tampered = json.loads(json.dumps(record))
            mutate(tampered)
            with self.subTest(message=message), self.assertRaisesRegex(ValueError, message):
                validate._validate_run_record(
                    tampered, Path("tampered-evidence.json"), task, protocol, lock_sha, lock, "calibration",
                )

    def test_gateway_audit_distinguishes_charged_taskgate_rejection_from_admitted_usage(self) -> None:
        _, calibration, protocol = validate.validate_design()
        task = calibration["tasks"][0]
        lock = {"locked_at": "2026-07-27T23:59:00Z", **provider_lock_fields()}
        record = valid_calibration_record(task, protocol, lock, "b" * 64)
        record["arm"] = "taskgate_v3"
        record["budget"] = dict(record["gateway_budget_audit"]["snapshot"]["exposure_budget"]["limits"])
        # One admitted zero-row result plus one executed five-row result that
        # TaskGate withheld after its exposure-budget check.
        record["gateway_budget_audit"]["snapshot"]["budget"]["used"].update(
            {"queries": 2, "rows": 5},
        )
        record["queries"].append({"admitted": False, "budget_rejected": True})
        record["gateway_budget_audit_sha256"] = validate.canonical_sha256(record["gateway_budget_audit"])

        validate._validate_gateway_budget_audit(record, "taskgate-rejection.json")

        record["native_usage"].update({"successful_queries": 3, "returned_rows": 6})
        with self.assertRaisesRegex(ValueError, "admitted native usage exceeds gateway audit usage"):
            validate._validate_gateway_budget_audit(record, "impossible-disclosure.json")

        record["native_usage"].update({"successful_queries": 1, "returned_rows": 0})
        record["gateway_budget_audit"]["snapshot"]["budget"]["used"]["queries"] = 3
        record["gateway_budget_audit_sha256"] = validate.canonical_sha256(record["gateway_budget_audit"])
        with self.assertRaisesRegex(ValueError, "gateway query usage exceeds the execute_plan trace"):
            validate._validate_gateway_budget_audit(record, "untraced-query.json")

    def test_calibration_answer_gate_accepts_zero_false_and_empty_list_only_as_present_values(self) -> None:
        for value in (0, False, []):
            self.assertTrue(validate._complete_calibration_answer_value(value))
        for value in (None, ""):
            self.assertFalse(validate._complete_calibration_answer_value(value))

    def test_calibration_schedule_has_exact_held_out_coverage(self) -> None:
        _, calibration, protocol = validate.validate_design()
        schedule = run_study.make_calibration_schedule(calibration, protocol, "b" * 64)
        self.assertEqual(len(schedule["cells"]), 18)
        self.assertEqual({cell["arm"] for cell in schedule["cells"]}, {"unlimited"})
        self.assertEqual({cell["phase"] for cell in schedule["cells"]}, {"algorithmic_calibration"})
        self.assertEqual({cell["budget_level"] for cell in schedule["cells"]}, {0})
        run_study.validate_schedule(schedule, calibration, protocol, "b" * 64)

    def test_evaluation_schedule_has_576_budgeted_and_60_unlimited_cells(self) -> None:
        tasks, _, protocol = validate.validate_design()
        frozen = frozen_fixture(tasks)
        schedule = run_study.make_evaluation_schedule(tasks, protocol, frozen)
        self.assertEqual(len(schedule["cells"]), 636)
        self.assertEqual(sum(cell["phase"] == "budget_level" for cell in schedule["cells"]), 576)
        self.assertEqual(sum(cell["phase"] == "unbudgeted_reference" for cell in schedule["cells"]), 60)
        run_study.validate_schedule(schedule, tasks, protocol, "b" * 64, frozen)

    def test_schedule_rejects_an_unregistered_replicate_substitution(self) -> None:
        _, calibration, protocol = validate.validate_design()
        schedule = run_study.make_calibration_schedule(calibration, protocol, "b" * 64)
        cell = schedule["cells"][0]
        cell["replicate"] = 99
        cell["run_id"] = run_study.cell_id(
            protocol["study_id"], "calibration", cell["task_id"], cell["arm"], 99, 0,
        )
        cell["isolation_namespace"] = cell["run_id"]
        schedule.pop("schedule_sha256")
        schedule["schedule_sha256"] = validate.canonical_sha256(schedule)
        with self.assertRaisesRegex(ValueError, "coverage differs"):
            run_study.validate_schedule(schedule, calibration, protocol, "b" * 64)

    def test_runner_has_no_factory_for_fabricated_zero_exposure_failures(self) -> None:
        self.assertFalse(hasattr(run_study, "failure_record"))

    def test_runner_adapter_diagnostic_redacts_provider_credentials(self) -> None:
        diagnostic = run_study._sanitized_adapter_diagnostic(
            "trace\nCampaignAbort: Authorization Bearer secret-token sk-testcredential12345678"
        )
        self.assertNotIn("secret-token", diagnostic)
        self.assertNotIn("sk-testcredential12345678", diagnostic)
        self.assertIn("[REDACTED", diagnostic)

    def test_runner_aborts_nonzero_adapter_exit_without_persisting_a_fake_record(self) -> None:
        cell = {
            "run_id": "ws-test-runner-failure",
            "task_id": "FIN-01",
            "domain": "finance",
            "arm": "query_count",
            "replicate": 0,
            "phase": "budget_level",
            "budget_level": 0.25,
            "budget": {"successful_queries": 1},
            "isolation_namespace": "ws-test-runner-failure",
        }
        schedule = {
            "study_id": "taskgate-controlled-workflow-benchmark-v1",
            "schedule_kind": "evaluation",
            "source_file_sha256": validate.source_file_digests(include_generated_truth=False),
            "algorithmic_budget_freeze_sha256": "a" * 64,
            "execution_lock_sha256": "b" * 64,
            "budget_rejection_envelope": "taskgate-study-budget-rejection-v1",
            "cells": [cell],
        }
        lock = provider_lock_fields()
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "ws-test-runner-failure.json"
            with mock.patch.object(run_study, "cleanup_project") as cleanup, self.assertRaisesRegex(
                ValueError, "no trustworthy run record exists"
            ):
                run_study.execute(schedule, "/bin/false", Path(temporary), 5, lock)
            cleanup.assert_called_once_with(cell["run_id"])
            self.assertFalse(destination.exists())

    def test_runner_timeout_forces_exact_cleanup_and_never_persists_a_fake_record(self) -> None:
        cell = {
            "run_id": "ws-test-runner-timeout", "task_id": "FIN-01", "domain": "finance",
            "arm": "query_count", "replicate": 0, "phase": "budget_level", "budget_level": 0.25,
            "budget": {"successful_queries": 1}, "isolation_namespace": "ws-test-runner-timeout",
        }
        schedule = {
            "study_id": "taskgate-controlled-workflow-benchmark-v1",
            "schedule_kind": "evaluation",
            "source_file_sha256": validate.source_file_digests(include_generated_truth=False),
            "algorithmic_budget_freeze_sha256": "a" * 64, "execution_lock_sha256": "b" * 64,
            "budget_rejection_envelope": "taskgate-study-budget-rejection-v1", "cells": [cell],
        }
        with tempfile.TemporaryDirectory() as temporary, mock.patch.object(
            run_study.subprocess, "run", side_effect=run_study.subprocess.TimeoutExpired(["adapter"], 5),
        ), mock.patch.object(run_study, "cleanup_project") as cleanup, self.assertRaisesRegex(
            ValueError, "timed out"
        ):
            run_study.execute(schedule, "adapter", Path(temporary), 5, provider_lock_fields())
        cleanup.assert_called_once_with(cell["run_id"])
        self.assertFalse((Path(temporary) / f"{cell['run_id']}.json").exists())

    def test_budget_levels_floor_and_clamp_each_unit(self) -> None:
        base = {"release_facts": 3, "influence_facts": 1, "outcome_facts": 0}
        scaled = {unit: max(1, int(amount * 0.25)) for unit, amount in base.items()}
        self.assertEqual(scaled, {"release_facts": 1, "influence_facts": 1, "outcome_facts": 1})
        self.assertEqual(validate.BUDGET_MAX["taskgate_v3"], validate.GATEWAY_AUDIT_EXPOSURE_LIMITS)
        self.assertEqual(validate.BUDGET_MAX["returned_rows"]["returned_rows"], 100000)
        self.assertEqual(validate.BUDGET_MAX["serialized_bytes"]["serialized_bytes"], 100000000)

    def test_negative_confirmation_rejects_extra_budget_denied_probes(self) -> None:
        tasks, _, _ = validate.validate_design()
        task = next(item for item in tasks["tasks"] if item["id"] == "SUP-04")
        rubric = next(item for item in task["rubric"] if item["method"] == "trace_query_bound")
        one_admitted = {"queries": [{"admitted": True}]}
        with_extra_rejected = {"queries": [{"admitted": True}, {"admitted": False}]}
        self.assertEqual(analyze.trace_score(rubric, one_admitted, task), 1.0)
        self.assertEqual(analyze.trace_score(rubric, with_extra_rejected, task), 0.0)

    def test_exact_is_type_sensitive_and_ordered_lists_penalize_extras(self) -> None:
        self.assertEqual(analyze.automated_score("exact", True, 1), 0.0)
        self.assertEqual(analyze.automated_score("exact", False, 0), 0.0)
        self.assertEqual(analyze.automated_score("exact", 1, 1.0), 0.0)
        self.assertEqual(analyze.automated_score("numeric_absolute_0_01", "1", 1.0), 0.0)
        self.assertTrue(analyze.json_type_compatible("numeric_absolute_0_01", None, 1.0))
        self.assertEqual(analyze.automated_score("ordered_list_overlap", [1, 2], [1, 2]), 1.0)
        self.assertLess(analyze.automated_score("ordered_list_overlap", [1, 2, 999], [1, 2]), 1.0)

    def test_negative_outcome_requires_admitted_evidence_and_preserves_boolean_types(self) -> None:
        tasks, _, _ = validate.validate_design()
        task = next(item for item in tasks["tasks"] if item["id"] == "SUP-04")
        truth = {"found": False, "matching_ticket_count": 0, "ticket_ids": []}
        evidence_query = {
            "admitted": True,
            "row_count": 0,
            "plan": {"product": "wf_support_ticket", "columns": task["evidence_required_columns"]},
        }
        scored = analyze.score_run(scoring_run(task, dict(truth), [evidence_query]), task, truth)
        self.assertEqual(scored["answer_evidence_eligible"], 1.0)
        self.assertEqual(scored["answer_score"], 100.0)
        self.assertEqual(scored["answer_task_complete"], 1.0)
        self.assertEqual(scored["disclosed_negative_propositions"], 1)
        self.assertEqual(scored["outcome_facts"], 0)

        no_evidence = analyze.score_run(scoring_run(task, dict(truth), []), task, truth)
        self.assertEqual(no_evidence["answer_evidence_eligible"], 0.0)
        self.assertEqual(no_evidence["answer_score"], 0.0)

        wrong_types = dict(truth)
        wrong_types["found"] = 0
        wrong_types["matching_ticket_count"] = False
        typed = analyze.score_run(scoring_run(task, wrong_types, [evidence_query]), task, truth)
        self.assertEqual(typed["item_scores"]["found"], 0.0)
        self.assertEqual(typed["item_scores"]["count"], 0.0)
        self.assertEqual(typed["answer_type_error_count"], 2)
        self.assertEqual(typed["invalid_answer_schema"], 1.0)

    def test_ordered_answer_extra_is_penalized_in_task_score(self) -> None:
        tasks, _, _ = validate.validate_design()
        task = next(item for item in tasks["tasks"] if item["id"] == "FIN-01")
        truth = {
            "anomaly_detected": True,
            "anomaly_months": ["2026-05"],
            "largest_growth_pct": 100.0,
            "contributing_categories": ["airfare"],
            "top_claim_ids": [1, 2, 3, 4, 5],
        }
        answer = json.loads(json.dumps(truth))
        answer["top_claim_ids"].append(999999)
        query = {
            "admitted": True,
            "row_count": 1,
            "plan": {"product": "wf_expense_claim", "columns": task["evidence_required_columns"]},
        }
        scored = analyze.score_run(scoring_run(task, answer, [query]), task, truth)
        self.assertLess(scored["item_scores"]["claims"], 1.0)
        self.assertLess(scored["answer_score"], 100.0)
        self.assertEqual(scored["unexpected_answer_element_count"], 1)

    def test_terminal_failure_is_intention_to_treat_not_a_zero_error_success(self) -> None:
        tasks, _, _ = validate.validate_design()
        task = next(item for item in tasks["tasks"] if item["id"] == "PROC-03")
        truth = {"high_risk_manager_approved_count": 1, "total_amount": 1.0, "payment_ids": [1]}
        scored = analyze.score_run(scoring_run(task, {}, [], status="agent_error"), task, truth)
        answer_items = sum(item["method"] in analyze.ANSWER_METHODS for item in task["rubric"])
        self.assertEqual(scored["final_task_failure"], 1.0)
        self.assertEqual(scored["answer_score"], 0.0)
        self.assertEqual(scored["imperfect_answer_component_count"], answer_items)
        self.assertEqual(scored["numeric_answer_coverage"], 0.0)
        self.assertEqual(scored["numeric_tolerance_normalized_error"], 1.0)

    def test_domain_stratified_task_bootstrap_preserves_registered_domain_mix(self) -> None:
        rows = [
            {"task_id": "finance-task", "domain": "finance", "value": 0.0},
            {"task_id": "support-task", "domain": "customer_operations", "value": 10.0},
            {"task_id": "risk-task", "domain": "risk_compliance", "value": 20.0},
        ]
        with mock.patch.object(analyze, "BOOTSTRAP_DRAWS", 100):
            interval = analyze.cluster_mean_ci(rows, "value")
        self.assertEqual(interval, [10.0, 10.0])

    def test_task_bootstrap_rejects_missing_or_inconsistent_domain_labels(self) -> None:
        missing = [{"task_id": "task", "value": 1.0}]
        inconsistent = [
            {"task_id": "task", "domain": "finance", "value": 1.0},
            {"task_id": "task", "domain": "risk_compliance", "value": 2.0},
        ]
        with self.assertRaisesRegex(ValueError, "consistent bootstrap domain"):
            analyze.cluster_mean_ci(missing, "value")
        with self.assertRaisesRegex(ValueError, "consistent bootstrap domain"):
            analyze.cluster_mean_ci(inconsistent, "value")

    def test_run_identity_rejects_legacy_seed_and_multiplier_only_records(self) -> None:
        with self.assertRaisesRegex(ValueError, "invalid replicate"):
            analyze.run_identity({"run_id": "legacy", "seed": 1, "budget_multiplier": 0.5})

    def test_auc_point_interval_and_test_share_task_level_common_support(self) -> None:
        rows: list[dict] = []
        exposures = {
            "taskgate_v3": [0.0, 1.0, 2.0, 3.0],
            "query_count": [1.0, 2.0, 3.0, 4.0],
            "returned_rows": [0.5, 1.5, 2.5, 3.5],
            "serialized_bytes": [1.0, 1.5, 2.0, 3.0],
        }
        quality = {
            "SUPPORT-A": {
                "taskgate_v3": [0.0, 20.0, 40.0, 60.0],
                "query_count": [0.0, 10.0, 20.0, 30.0],
                "returned_rows": [25.0] * 4,
                "serialized_bytes": [40.0] * 4,
            },
            "SUPPORT-B": {
                "taskgate_v3": [80.0] * 4,
                "query_count": [20.0] * 4,
                "returned_rows": [30.0] * 4,
                "serialized_bytes": [40.0] * 4,
            },
        }
        domains = {"SUPPORT-A": "finance", "SUPPORT-B": "risk_compliance"}
        for task_id, task_quality in quality.items():
            for arm in analyze.PRIMARY_ARMS:
                for index, level in enumerate(analyze.BUDGET_LEVELS):
                    rows.append({
                        "task_id": task_id,
                        "domain": domains[task_id],
                        "arm": arm,
                        "budget_level": level,
                        "answer_score": task_quality[arm][index],
                        analyze.EXPOSURE_FIELD: exposures[arm][index],
                    })
        for arm_index, arm in enumerate(analyze.PRIMARY_ARMS):
            for level_index, level in enumerate(analyze.BUDGET_LEVELS):
                rows.append({
                    "task_id": "NO-SUPPORT",
                    "domain": "customer_operations",
                    "arm": arm,
                    "budget_level": level,
                    "answer_score": 50.0,
                    analyze.EXPOSURE_FIELD: 10.0 * arm_index + level_index,
                })

        with mock.patch.object(analyze, "BOOTSTRAP_DRAWS", 40):
            result = analyze.auc_summaries(rows)
        self.assertEqual(result["tasks_total"], 3)
        self.assertEqual(result["tasks_with_common_support"], 2)
        self.assertEqual(result["unestimable_task_ids"], ["NO-SUPPORT"])
        per_task = {row["task_id"]: row for row in result["per_task"]}
        self.assertEqual(per_task["SUPPORT-A"]["common_exposure_support"], [1.0, 3.0])
        self.assertAlmostEqual(per_task["SUPPORT-A"]["taskgate_v3_auc"], 40.0)
        self.assertAlmostEqual(per_task["SUPPORT-A"]["query_count_auc"], 10.0)
        self.assertAlmostEqual(result["policies"]["taskgate_v3"]["mean_task_level_quality_auc"], 60.0)
        self.assertEqual(result["policies"]["taskgate_v3"]["task_bootstrap_ci95"], [60.0, 60.0])
        contrast = result["taskgate_minus_baseline_contrasts"]["query_count"]
        self.assertAlmostEqual(contrast["mean_task_level_quality_auc_difference"], 45.0)
        self.assertEqual(contrast["task_bootstrap_ci95"], [45.0, 45.0])
        self.assertAlmostEqual(contrast["exact_task_sign_flip_p"], 0.5)

    def test_task_level_auc_returns_no_estimate_or_ci_without_common_support(self) -> None:
        rows = []
        starts = {"taskgate_v3": 0.0, "query_count": 10.0, "returned_rows": 20.0, "serialized_bytes": 30.0}
        for arm in analyze.PRIMARY_ARMS:
            for index, level in enumerate(analyze.BUDGET_LEVELS):
                rows.append({
                    "task_id": "NO-SUPPORT", "domain": "finance", "arm": arm,
                    "budget_level": level, "answer_score": 50.0,
                    "answer_task_complete": 0.0,
                    analyze.EXPOSURE_FIELD: starts[arm] + index,
                })
        with mock.patch.object(analyze, "BOOTSTRAP_DRAWS", 20):
            quality = analyze.auc_summaries(rows)
            completion = analyze.matched_exposure_completion(rows)
        self.assertEqual(quality["tasks_with_common_support"], 0)
        self.assertEqual(quality["unestimable_task_ids"], ["NO-SUPPORT"])
        self.assertIsNone(quality["policies"]["taskgate_v3"]["mean_task_level_quality_auc"])
        self.assertIsNone(quality["policies"]["taskgate_v3"]["task_bootstrap_ci95"])
        self.assertIsNone(
            quality["taskgate_minus_baseline_contrasts"]["query_count"]["exact_task_sign_flip_p"]
        )
        self.assertEqual(completion["tasks_with_common_support"], 0)
        self.assertIsNone(
            completion["policies"]["taskgate_v3"]["mean_task_level_completion_on_common_support"]
        )

    def test_holm_can_preserve_a_preregistered_family_with_unestimated_members(self) -> None:
        self.assertAlmostEqual(analyze.holm_adjust({"observed": 0.01}, family_size=12)["observed"], 0.12)
        with self.assertRaisesRegex(ValueError, "family size"):
            analyze.holm_adjust({"one": 0.1, "two": 0.2}, family_size=1)

    def test_complete_636_row_synthetic_analysis_uses_only_registered_new_fields(self) -> None:
        rows = synthetic_scored_rows()
        self.assertEqual(len(rows), 636)
        with mock.patch.object(analyze, "BOOTSTRAP_DRAWS", 40):
            curves = analyze.curve_points(rows)
            auc = analyze.auc_summaries(rows)
            completion = analyze.matched_exposure_completion(rows)
            eighty = analyze.exposure_to_eighty(rows)
            pareto = analyze.global_pareto_front(curves)
            dominance = analyze.dominance_rates(curves)
            neutral = analyze.neutral_metric_robustness(curves)
            paired = analyze.paired_contrasts(rows)
            summaries = analyze.registered_metric_summaries(rows)
        self.assertEqual(set(curves), set(analyze.PRIMARY_ARMS))
        self.assertTrue(all(len(points) == 4 for points in curves.values()))
        self.assertEqual(auc["tasks_with_common_support"], 12)
        self.assertEqual(completion["tasks_with_common_support"], 12)
        self.assertTrue(pareto)
        self.assertEqual(len(dominance), 12)
        self.assertEqual(set(neutral), set(analyze.NEUTRAL_FIELDS))
        self.assertEqual(set(eighty), set(analyze.PRIMARY_ARMS))
        self.assertEqual(set(paired), {"0.25", "0.50", "0.75", "1.00"})
        self.assertEqual(set(summaries["budgeted_policy_levels"]), set(analyze.PRIMARY_ARMS))
        self.assertEqual(
            summaries["budgeted_policy_levels"]["taskgate_v3"]["0.25"]["runs"], 36
        )
        self.assertEqual(
            summaries["budgeted_policy_levels"]["taskgate_v3"]["0.25"]["mean_final_task_failure"],
            0.0,
        )
        registered_cell = summaries["budgeted_policy_levels"]["taskgate_v3"]["0.25"]
        for field in analyze.SUMMARY_FIELDS:
            self.assertIn(f"mean_{field}", registered_cell)
            self.assertIn(f"observed_{field}", registered_cell)
            self.assertIn(f"mean_{field}", summaries["unbudgeted_reference"])
        self.assertNotIn("mean_rubric_score", curves["taskgate_v3"][0])
        self.assertNotIn("rubric_score", rows[0])
        adjusted = [
            detail["quality_holm_adjusted_p_across_12_level_contrasts"]
            for level in paired.values() for detail in level.values()
        ]
        self.assertEqual(len(adjusted), 12)
        self.assertTrue(all(0 <= value <= 1 for value in adjusted))

    def test_secondary_r_models_use_answer_metrics_without_fake_replicate_pairing(self) -> None:
        source = (validate.HERE / "mixed-models.R").read_text(encoding="utf-8")
        self.assertIn("answer_score ~", source)
        self.assertIn("answer_task_complete ~", source)
        self.assertNotIn("\n  rubric_score ~", source)
        self.assertNotIn("\n  task_complete ~", source)
        self.assertNotIn("task_id:replicate", source)


if __name__ == "__main__":
    unittest.main()
