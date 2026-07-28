#!/usr/bin/env python3

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import analyze
import controller
import deepseek_agent_adapter
import freeze_budgets
import run_study
import validate


class WorkflowStudyDesignTest(unittest.TestCase):
    def test_registered_design_is_complete_but_uncollected(self) -> None:
        tasks, protocol = validate.validate_design()
        self.assertEqual(len(tasks["tasks"]), 12)
        self.assertEqual(
            {task["domain"] for task in tasks["tasks"]},
            {"finance", "customer_operations", "risk_compliance"},
        )
        self.assertEqual(protocol["sampling"]["planned_agent_runs"], 444)
        self.assertEqual(protocol["status"], "designed_not_collected")

    def test_automated_scoring_methods(self) -> None:
        self.assertEqual(analyze.automated_score("exact", True, True), 1)
        self.assertEqual(analyze.automated_score("numeric_absolute_0_01", "10.01", 10), 1)
        self.assertAlmostEqual(analyze.automated_score("set_f1", [1, 2], [2, 3]), 0.5)
        self.assertEqual(analyze.automated_score("ordered_list_overlap", [1, 9, 3], [1, 2, 3]), 2 / 3)

    def test_incomplete_collection_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            with self.assertRaisesRegex(ValueError, "no independent expert budget"):
                validate.validate_budgets(directory, {"FIN-01"}, 3)

    def test_exported_truth_requires_exact_task_coverage(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "truth.json"
            path.write_text(json.dumps({"FIN-01": {"answer": 1}}), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "does not cover exactly"):
                validate.validate_truth(path, {"FIN-01", "FIN-02"})

    def test_query_baseline_charges_zero_result_and_deduplicates_transport_retry(self) -> None:
        policy = controller.BaselineController("query_count", 1)
        payload = controller.canonical_response_bytes({"rows": []})
        first = policy.admit("request-1", 0, payload)
        retry = policy.admit("request-1", 0, payload)
        rejected = policy.admit("request-2", 0, payload)
        self.assertTrue(first.released)
        self.assertEqual(first.charged, 1)
        self.assertTrue(retry.idempotent_retry)
        self.assertEqual(retry.charged, 0)
        self.assertFalse(rejected.released)
        self.assertEqual(policy.runtime_budget_rejections, 1)

    def test_row_and_byte_baselines_buffer_before_release(self) -> None:
        empty = controller.canonical_response_bytes({"rows": []})
        scalar_zero = controller.canonical_response_bytes({"rows": [{"count": 0}]})
        row_policy = controller.BaselineController("returned_rows", 0)
        self.assertTrue(row_policy.admit("empty", 0, empty).released)
        self.assertFalse(row_policy.admit("scalar", 1, scalar_zero).released)
        byte_policy = controller.BaselineController("serialized_bytes", len(empty) - 1)
        decision = byte_policy.admit("empty", 0, empty)
        self.assertFalse(decision.released)
        self.assertEqual(byte_policy.used, 0)

    def test_request_id_cannot_be_rebound(self) -> None:
        policy = controller.BaselineController("returned_rows", 10)
        policy.admit("request-1", 1, b"first")
        with self.assertRaisesRegex(ValueError, "different buffered result"):
            policy.admit("request-1", 1, b"second")

    def test_paired_contrasts_and_holm_adjustment(self) -> None:
        rows = [
            {"task_id": "FIN-01", "seed": 0, "arm": "taskgate_v3", "score": 90},
            {"task_id": "FIN-01", "seed": 0, "arm": "query_count", "score": 70},
            {"task_id": "FIN-02", "seed": 0, "arm": "taskgate_v3", "score": 80},
            {"task_id": "FIN-02", "seed": 0, "arm": "query_count", "score": 75},
        ]
        differences = analyze.paired_differences(rows, "query_count", "score")
        self.assertEqual([row["difference"] for row in differences], [20, 5])
        adjusted = analyze.holm_adjust({"a": 0.01, "b": 0.04, "c": 0.03})
        self.assertEqual(adjusted, {"a": 0.03, "c": 0.06, "b": 0.06})

    def test_registered_schedule_has_independent_primary_upper_bound_and_pareto_cells(self) -> None:
        tasks, protocol = validate.validate_design()
        frozen = {
            "freeze_sha256": "a" * 64,
            "execution_lock_sha256": "b" * 64,
            "budgets": {
                task["id"]: {
                    "taskgate_v3": {"release_facts": 3, "influence_facts": 5, "outcome_facts": 1},
                    "query_count": {"successful_queries": 4},
                    "returned_rows": {"returned_rows": 10},
                    "serialized_bytes": {"serialized_bytes": 1000},
                }
                for task in tasks["tasks"]
            },
        }
        schedule = run_study.make_schedule(tasks, protocol, frozen)
        self.assertEqual(len(schedule["cells"]), 444)
        self.assertEqual(sum(cell["phase"] == "primary" for cell in schedule["cells"]), 240)
        self.assertEqual(sum(cell["phase"] == "unlimited_upper_bound" for cell in schedule["cells"]), 60)
        self.assertEqual(sum(cell["phase"] == "pareto_sweep" for cell in schedule["cells"]), 144)
        taskgate_half = next(
            cell for cell in schedule["cells"]
            if cell["task_id"] == "FIN-01" and cell["arm"] == "taskgate_v3" and cell["budget_multiplier"] == 0.5
        )
        self.assertEqual(taskgate_half["budget"]["outcome_facts"], 1)

    def test_audit_risk_uses_only_linked_fact_identities(self) -> None:
        task = next(task for task in validate.load(validate.TASKS)["tasks"] if task["id"] == "FIN-02")
        facts = [
            {"ledger_kind": "RELEASE", "identity": {"kind": "base-cell", "source_namespace": "workflow.finance.expense_claim", "entity_key": "1", "field": "amount"}},
            {"ledger_kind": "INFLUENCE", "identity": {"kind": "base-cell", "source_namespace": "workflow.finance.expense_claim", "entity_key": "1", "field": "employee_name"}},
            {"ledger_kind": "OUTCOME", "identity": {"kind": "outcome"}},
        ]
        risk = deepseek_agent_adapter.risk_metrics(facts, task)
        self.assertEqual(risk["release_facts"], 1)
        self.assertEqual(risk["outcome_facts"], 1)
        self.assertEqual(risk["distinct_sensitive_records"], 1)
        self.assertEqual(risk["unnecessary_sensitive_fields"], 1)

    def test_budget_freeze_uses_lower_median_and_reports_rank_agreement(self) -> None:
        self.assertEqual(freeze_budgets.lower_median([9, 3, 6]), 6)
        self.assertEqual(freeze_budgets.lower_median([9, 3, 6, 8]), 6)
        self.assertEqual(freeze_budgets.kendalls_w([[1, 2, 3], [10, 20, 30]]), 1.0)


if __name__ == "__main__":
    unittest.main()
