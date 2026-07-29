#!/usr/bin/env python3

from __future__ import annotations

import contextlib
import csv
import datetime as dt
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import certify_collection
import run_study
import validate


def _write_json(path: Path, value: object) -> bytes:
    encoded = (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(encoded)
    return encoded


def _fake_freeze(lock_sha: str, truth_sha: str) -> dict:
    base = {
        "taskgate_v3": {"release_facts": 20, "influence_facts": 40, "outcome_facts": 4},
        "query_count": {"successful_queries": 8},
        "returned_rows": {"returned_rows": 80},
        "serialized_bytes": {"serialized_bytes": 8000},
    }
    domains = {}
    for domain in validate.DOMAINS:
        domains[domain] = {
            "levels": {
                validate.level_key(level): {
                    arm: {unit: max(1, int(amount * level)) for unit, amount in budget.items()}
                    for arm, budget in base.items()
                }
                for level in validate.BUDGET_LEVELS
            }
        }
    return {
        "freeze_sha256": "f" * 64,
        "execution_lock_sha256": lock_sha,
        "frozen_at": "2026-07-28T00:02:00Z",
        "status": "frozen_from_held_out_calibration",
        "source_file_sha256": {
            "raw/ground-truth.json": truth_sha,
            "synthetic-fixture": "a" * 64,
        },
        "domains": domains,
    }


class CertificationFixture:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.truth = root / "ground-truth.json"
        self.freeze_path = root / "algorithmic-budget-freeze.json"
        self.lock_path = root / "execution-lock.json"
        self.calibration_schedule_path = root / "calibration-schedule.json"
        self.evaluation_schedule_path = root / "evaluation-schedule.json"
        self.calibration_runs_path = root / "calibration-runs"
        self.evaluation_runs_path = root / "evaluation-runs"
        self.results_path = root / "results.json"
        self.csv_path = root / "scored-runs.csv"
        self.output_path = root / "collection-certificate.json"
        self.calibration_runs_path.mkdir()
        self.evaluation_runs_path.mkdir()

        tasks_doc, calibration_doc, self.protocol = validate.validate_design()
        self.plan = validate.sampling_plan(self.protocol)
        _write_json(self.truth, {"fixture": True})
        self.lock = {
            "schema_version": 2,
            "study_id": self.protocol["study_id"],
            "campaign_id": "certifier-unit-test",
            "locked_at": "2026-07-27T23:59:00Z",
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
                "max_attempts": 4,
                "initial_backoff_seconds": 1,
                "max_backoff_seconds": 8,
                "retryable_http_statuses": [429, 500, 502, 503, 504],
                "retry_insufficient_system_resource": True,
            },
            "infrastructure_retry": {
                "compose_start_max_attempts": 3,
                "compose_start_backoff_seconds": 2.0,
            },
            "pricing_source": "https://api-docs.deepseek.com/quick_start/pricing/",
            "pricing_usd_per_million_tokens": {
                "prompt_cache_hit": 0.0028,
                "prompt_cache_miss": 0.14,
                "completion": 0.28,
            },
            "phase_cost_limits_usd": {"calibration": 1.0, "evaluation": 10.0},
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
        }
        _write_json(self.lock_path, self.lock)
        lock_sha = validate.file_sha256(self.lock_path)
        self.frozen = _fake_freeze(lock_sha, validate.file_sha256(self.truth))
        _write_json(self.freeze_path, self.frozen)

        calibration_schedule = run_study.make_calibration_schedule(
            calibration_doc, self.protocol, lock_sha,
        )
        evaluation_schedule = run_study.make_evaluation_schedule(
            tasks_doc, self.protocol, self.frozen,
        )
        _write_json(self.calibration_schedule_path, calibration_schedule)
        _write_json(self.evaluation_schedule_path, evaluation_schedule)

        self.calibration_records = []
        calibration_start = dt.datetime(2026, 7, 28, 0, 0, tzinfo=dt.timezone.utc)
        for index, cell in enumerate(calibration_schedule["cells"]):
            started = calibration_start + dt.timedelta(seconds=2 * index)
            record = {
                "run_id": cell["run_id"], "task_id": cell["task_id"],
                "arm": cell["arm"], "replicate": cell["replicate"],
                "phase": cell["phase"], "budget_level": cell["budget_level"],
                "status": "completed",
                "started_at": started.isoformat().replace("+00:00", "Z"),
                "finished_at": (started + dt.timedelta(seconds=1)).isoformat().replace("+00:00", "Z"),
                "provider_api": {"estimated_cost_usd": 0.001},
            }
            self.calibration_records.append(record)
            _write_json(self.calibration_runs_path / f"{cell['run_id']}.json", record)

        statuses = ("completed", "completed", "completed", "budget_exhausted", "tool_error", "agent_error")
        self.evaluation_records = []
        scored = []
        evaluation_start = dt.datetime(2026, 7, 28, 0, 3, tzinfo=dt.timezone.utc)
        for index, cell in enumerate(evaluation_schedule["cells"]):
            status = statuses[index % len(statuses)]
            started = evaluation_start + dt.timedelta(seconds=2 * index)
            record = {
                "run_id": cell["run_id"], "task_id": cell["task_id"],
                "arm": cell["arm"], "replicate": cell["replicate"],
                "phase": cell["phase"], "budget_level": cell["budget_level"],
                "status": status,
                "started_at": started.isoformat().replace("+00:00", "Z"),
                "finished_at": (started + dt.timedelta(seconds=1)).isoformat().replace("+00:00", "Z"),
                "provider_api": {"estimated_cost_usd": 0.001},
            }
            self.evaluation_records.append(record)
            _write_json(self.evaluation_runs_path / f"{cell['run_id']}.json", record)
            scored.append({
                **record,
                "answer_task_complete": float(status == "completed" and index % 2 == 0),
                "workflow_task_complete": float(status == "completed" and index % 3 == 0),
            })

        self.result = {
            "schema_version": 3,
            "study_id": self.protocol["study_id"],
            "status": "complete_registered_collection",
            "scoring": {
                "type": "deterministic_policy_blind_automatic",
                "human_or_llm_judge_inputs": False,
                "task_manifest_sha256": validate.file_sha256(validate.TASKS),
                "truth_sha256": validate.file_sha256(self.truth),
                "analysis_sha256": validate.file_sha256(validate.HERE / "analyze.py"),
            },
            "execution_lock": {
                "sha256": lock_sha,
                "provider": self.lock["provider"],
                "model": self.lock["model"],
                "model_version": self.lock["model_version"],
                "thinking_mode": self.lock["thinking_mode"],
            },
            "algorithmic_budget_freeze_sha256": self.frozen["freeze_sha256"],
            "evaluation_runs": self.plan["planned_evaluation_runs"],
            "automatic_quality_exposure_benchmark": {
                "quality_exposure_auc": {
                    "tasks_total": 12,
                    "tasks_with_common_support": 10,
                    "unestimable_task_ids": ["SYN-1", "SYN-2"],
                },
                "critical_completion_at_matched_exposure": {
                    "tasks_total": 12,
                    "tasks_with_common_support": 9,
                    "unestimable_task_ids": ["SYN-1", "SYN-2", "SYN-3"],
                },
            },
            "scored_runs": scored,
        }
        self.scored = scored
        self.write_candidates()

    def write_candidates(self) -> None:
        self.expected_results = _write_json(self.results_path, self.result)
        buffer = io.StringIO(newline="")
        writer = csv.DictWriter(buffer, fieldnames=list(self.scored[0]))
        writer.writeheader()
        writer.writerows(self.scored)
        self.expected_csv = buffer.getvalue().encode("utf-8")
        self.csv_path.write_bytes(self.expected_csv)

    def reproduce(self, **kwargs: object) -> None:
        Path(kwargs["output"]).write_bytes(self.expected_results)
        Path(kwargs["scored_csv"]).write_bytes(self.expected_csv)

    def arguments(self) -> dict:
        return {
            "truth": self.truth,
            "freeze": self.freeze_path,
            "calibration_schedule": self.calibration_schedule_path,
            "calibration_runs": self.calibration_runs_path,
            "evaluation_schedule": self.evaluation_schedule_path,
            "execution_lock": self.lock_path,
            "evaluation_runs": self.evaluation_runs_path,
            "results": self.results_path,
            "scored_csv": self.csv_path,
            "analysis_timeout_seconds": 30,
        }


@contextlib.contextmanager
def patched_collection(fixture: CertificationFixture, rerun: object | None = None):
    with (
        mock.patch.object(validate, "validate_truth"),
        mock.patch.object(validate, "validate_execution_lock", return_value=fixture.lock),
        mock.patch.object(validate, "validate_calibration_runs", return_value=fixture.calibration_records),
        mock.patch.object(validate, "validate_algorithmic_freeze", return_value=fixture.frozen),
        mock.patch.object(validate, "validate_runs", return_value=fixture.evaluation_records),
        mock.patch.object(
            certify_collection, "rerun_analysis", side_effect=rerun or fixture.reproduce,
        ),
        mock.patch.object(certify_collection, "_utc_timestamp", return_value="2026-07-28T01:00:00Z"),
    ):
        yield


class CollectionCertificationTest(unittest.TestCase):
    def test_reanalysis_uses_frozen_hash_seed_and_only_the_offline_analyzer(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "reproduced.json"
            scored_csv = root / "reproduced.csv"

            def offline_process(command: list[str], **kwargs: object):
                output.write_text("{}\n", encoding="utf-8")
                scored_csv.write_text("run_id\n", encoding="utf-8")
                self.assertIn("analyze.py", command[1])
                self.assertNotIn("deepseek_agent_adapter.py", " ".join(command))
                self.assertEqual(kwargs["env"]["PYTHONHASHSEED"], "0")
                self.assertNotIn("DEEPSEEK_API_KEY", kwargs["env"])
                return certify_collection.subprocess.CompletedProcess(command, 0, "ok", "")

            with mock.patch.object(certify_collection.subprocess, "run", side_effect=offline_process):
                certify_collection.rerun_analysis(
                    truth=root / "truth.json", freeze=root / "freeze.json",
                    calibration_runs=root / "calibration", execution_lock=root / "lock.json",
                    evaluation_runs=root / "runs", output=output, scored_csv=scored_csv,
                    timeout_seconds=30,
                )

    def test_complete_collection_emits_self_digesting_atomic_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            with patched_collection(fixture):
                certificate = certify_collection.certify_collection(**fixture.arguments())
            certify_collection.write_atomic_manifest(fixture.output_path, certificate)
            published = json.loads(fixture.output_path.read_text(encoding="utf-8"))

        digest_payload = dict(published)
        claimed = digest_payload.pop("manifest_sha256")
        self.assertEqual(claimed, validate.canonical_sha256(digest_payload))
        self.assertTrue(published["collection_complete"])
        self.assertEqual(published["registered_counts"], {
            "calibration_runs": 18, "evaluation_runs": 636, "total_agent_runs": 654,
        })
        self.assertEqual(sum(published["evaluation_status_counts"].values()), 636)
        self.assertEqual(
            published["evaluation_arm_level_counts"]["taskgate_v3"]["0.25"], 36,
        )
        self.assertEqual(
            published["evaluation_arm_level_counts"]["unlimited"]["unbudgeted_reference"], 60,
        )
        self.assertEqual(len(published["artifact_bindings"]["calibration_run_records"]), 18)
        self.assertEqual(len(published["artifact_bindings"]["evaluation_run_records"]), 636)
        self.assertTrue(published["deterministic_analysis"]["results_byte_for_byte_match"])
        self.assertTrue(published["deterministic_analysis"]["scored_csv_byte_for_byte_match"])

    def test_missing_raw_evaluation_record_blocks_certification(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            next(fixture.evaluation_runs_path.glob("*.json")).unlink()
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "manifest has 635 JSON files; expected exactly 636",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_extra_example_named_json_record_blocks_certification(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            _write_json(fixture.evaluation_runs_path / "hidden.example.extra.json", {"extra": True})
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "manifest has 637 JSON files; expected exactly 636",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_truth_must_match_the_copy_bound_into_the_freeze(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            _write_json(fixture.truth, {"fixture": "different-but-structurally-accepted"})
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "certification truth differs from the truth bound",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_tampered_schedule_digest_blocks_certification(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            schedule = json.loads(fixture.evaluation_schedule_path.read_text(encoding="utf-8"))
            schedule["cells"][0]["sequence_position"] = 999
            _write_json(fixture.evaluation_schedule_path, schedule)
            with patched_collection(fixture), self.assertRaisesRegex(ValueError, "schedule digest mismatch"):
                certify_collection.certify_collection(**fixture.arguments())

    def test_raw_record_replacement_during_reanalysis_blocks_certification(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))

            def reproduce_then_tamper(**kwargs: object) -> None:
                fixture.reproduce(**kwargs)
                first = sorted(fixture.evaluation_runs_path.glob("*.json"))[0]
                _write_json(first, {"tampered": True})

            with patched_collection(fixture, reproduce_then_tamper), self.assertRaisesRegex(
                ValueError, "collection inputs changed during deterministic reanalysis",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_schedule_timestamp_order_violation_blocks_certification(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            second = fixture.evaluation_records[1]
            second["started_at"] = fixture.evaluation_records[0]["started_at"]
            second["finished_at"] = fixture.evaluation_records[0]["finished_at"]
            _write_json(fixture.evaluation_runs_path / f"{second['run_id']}.json", second)
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "timestamps do not follow the registered schedule",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_locked_phase_cost_ceiling_is_recomputed_offline(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            first = fixture.evaluation_records[0]
            first["provider_api"]["estimated_cost_usd"] = 11.0
            _write_json(fixture.evaluation_runs_path / f"{first['run_id']}.json", first)
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "cumulative provider cost exceeds its locked ceiling",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_stale_but_structurally_valid_results_block_certification(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            fixture.results_path.write_bytes(fixture.expected_results.rstrip(b"\n") + b"  \n")
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "candidate results are stale or differ from deterministic reanalysis",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_partial_scored_result_cannot_be_labeled_complete(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            result = json.loads(fixture.results_path.read_text(encoding="utf-8"))
            result["scored_runs"].pop()
            _write_json(fixture.results_path, result)
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "scored_runs is not the complete 636-run collection",
            ):
                certify_collection.certify_collection(**fixture.arguments())

    def test_all_failed_evaluation_cells_are_complete_collection_not_successes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            for record, scored in zip(fixture.evaluation_records, fixture.scored):
                record["status"] = "agent_error"
                scored["status"] = "agent_error"
                scored["answer_task_complete"] = 0.0
                scored["workflow_task_complete"] = 0.0
                _write_json(fixture.evaluation_runs_path / f"{record['run_id']}.json", record)
            fixture.write_candidates()
            with patched_collection(fixture):
                certificate = certify_collection.certify_collection(**fixture.arguments())
        self.assertTrue(certificate["collection_complete"])
        self.assertEqual(certificate["evaluation_status_counts"]["agent_error"], 636)
        self.assertEqual(certificate["successful_workflows"]["execution_status_completed"], 0)
        self.assertEqual(certificate["successful_workflows"]["automatic_answer_task_complete"], 0)

    def test_verify_existing_recomputes_evidence_instead_of_trusting_self_digest(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            with patched_collection(fixture):
                certificate = certify_collection.certify_collection(**fixture.arguments())
                certify_collection.write_atomic_manifest(fixture.output_path, certificate)
                verified = certify_collection.verify_existing_manifest(fixture.output_path, certificate)
                self.assertEqual(verified, certificate)

                tampered = json.loads(fixture.output_path.read_text(encoding="utf-8"))
                tampered["successful_workflows"]["execution_status_completed"] += 1
                tampered.pop("manifest_sha256")
                tampered["manifest_sha256"] = validate.canonical_sha256(tampered)
                _write_json(fixture.output_path, tampered)
                with self.assertRaisesRegex(
                    ValueError, "differs from freshly validated artifacts",
                ):
                    certify_collection.verify_existing_manifest(fixture.output_path, certificate)

    def test_tampered_scored_csv_blocks_certification(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            fixture = CertificationFixture(Path(temporary))
            fixture.csv_path.write_bytes(fixture.expected_csv + b"\n")
            with patched_collection(fixture), self.assertRaisesRegex(
                ValueError, "candidate scored CSV is stale or differs from deterministic reanalysis",
            ):
                certify_collection.certify_collection(**fixture.arguments())


if __name__ == "__main__":
    unittest.main()
