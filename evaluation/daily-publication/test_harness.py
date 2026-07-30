import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest import mock


MODULE_PATH = pathlib.Path(__file__).with_name("harness.py")
SPEC = importlib.util.spec_from_file_location("daily_publication_harness", MODULE_PATH)
assert SPEC and SPEC.loader
HARNESS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HARNESS)


def digest(label: str) -> str:
    return hashlib.sha256(label.encode("utf-8")).hexdigest()


def online_value(rows: int = 2000) -> dict:
    publications = []
    for index, day in enumerate(HARNESS.DAYS):
        value = {
            "day": day,
            "publication_name": f"daily-lineitem-{day}-r{rows}",
            "row_count": rows,
        }
        for name in HARNESS.ONLINE_PUBLICATION_DIGEST_FIELDS:
            value[name] = digest(f"{name}-{day}")
        value["publication_manifest_digest"] = digest(f"publication-{index}")
        value["direct_result_sha256"] = digest(f"result-{index}")
        publications.append(value)
    transitions = []
    for index in range(3):
        old_publication = publications[index]
        new_publication = publications[index + 1]
        new_cache = digest(f"new-cache-{index}")
        root = f"root-{index}"
        transitions.append({
            "from": f"day{index}",
            "to": f"day{index + 1}",
            "switch_wall_ms": 2.0 + index,
            "first_query_wall_ms": 8.0 + index,
            "replay_wall_ms": 3.0 + index,
            "old_task": {
                "publication_digest_before": old_publication["publication_manifest_digest"],
                "publication_digest_after": old_publication["publication_manifest_digest"],
                "expected_publication_digest": old_publication["publication_manifest_digest"],
                "result_sha256_before": old_publication["direct_result_sha256"],
                "result_sha256_after": old_publication["direct_result_sha256"],
                "expected_result_sha256": old_publication["direct_result_sha256"],
            },
            "new_task": {
                "publication_digest": new_publication["publication_manifest_digest"],
                "expected_publication_digest": new_publication["publication_manifest_digest"],
                "result_sha256": new_publication["direct_result_sha256"],
                "expected_result_sha256": new_publication["direct_result_sha256"],
            },
            "old_ledger": {
                "before_switch_sha256": digest(f"ledger-{index}"),
                "after_switch_sha256": digest(f"ledger-{index}"),
            },
            "cache": {
                "old_cache_key_sha256": digest(f"old-cache-{index}"),
                "first_new_cache_key_sha256": new_cache,
                "first_new_semantic_replay": False,
                "replay_new_cache_key_sha256": new_cache,
                "replay_new_semantic_replay": True,
            },
            "delegation": {
                "root_task_id": root,
                "child_root_task_id": root,
                "child_parent_task_id": root,
                "root_publication_digest": new_publication["publication_manifest_digest"],
                "child_publication_digest": new_publication["publication_manifest_digest"],
            },
        })
    return {
        "schema_version": HARNESS.ONLINE_SCHEMA,
        "routing_model": HARNESS.ONLINE_ROUTING_MODEL,
        "rows_per_publication": rows,
        "measurement_boundary": HARNESS.ONLINE_MEASUREMENT_BOUNDARY,
        "fixture": {
            "fixture_class": "correctness_fixture",
            "rows_per_publication": rows,
            "generator_sha256": digest("generator"),
            "config_sha256": digest("config"),
            "dataset_manifest_sha256": digest(f"dataset-{rows}"),
            "publications": publications,
        },
        "transitions": transitions,
    }


class HarnessTest(unittest.TestCase):
    def setUp(self) -> None:
        self.config_path = pathlib.Path(__file__).with_name("config.json")

    def test_config_and_rendered_scale_coordinates(self) -> None:
        config = HARNESS.load_json(self.config_path)
        HARNESS.validate_config(config)
        HARNESS.validate_rows(config, 2000)
        HARNESS.validate_rows(config, 345000)
        with self.assertRaises(HARNESS.EvidenceError):
            HARNESS.validate_rows(config, 2001)
        value = HARNESS.snapshot_input("day3", 345000)
        self.assertEqual(value["source_relation"], "reporting.daily_lineitem_day3")
        self.assertEqual(value["snapshot"]["snapshot"], "rq5-daily-lineitem-day3-rows-345000")

    def test_approval_uses_only_measured_build_digests(self) -> None:
        candidate = HARNESS.snapshot_input("day0", 2000)
        publication = {
            "publication_name": candidate["publication_name"],
            "sidecar_digest": digest("sidecar"),
            "dictionary_digest": digest("dictionary"),
            "manifest_digest": digest("manifest"),
            "cold_payload_digest": digest("cold"),
            "hot_index_digest": digest("hot"),
        }
        phase = {
            "schema_version": HARNESS.PHASE_SCHEMA,
            "status": "pass",
            "phase": "build",
            "day": "day0",
            "sample": 0,
            "exit_code": 0,
            "command_report": {"mode": "build", "publications": [publication]},
        }
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            candidate_path = root / "candidate.json"
            report_path = root / "report.json"
            output_path = root / "approved.json"
            candidate_path.write_text(json.dumps(candidate), encoding="utf-8")
            report_path.write_text(json.dumps(phase), encoding="utf-8")
            HARNESS.approve_input(candidate_path, report_path, output_path)
            approved = HARNESS.load_json(output_path)
        self.assertEqual(approved["expected_digests"]["manifest_digest"], digest("manifest"))
        self.assertNotIn("expected_digests", candidate)

    def test_online_contract_derives_all_five_conditions(self) -> None:
        evidence = online_value()
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "online.json"
            path.write_text(json.dumps(evidence), encoding="utf-8")
            result, gates = HARNESS.online_evidence(path)
        self.assertEqual(result["status"], "complete")
        self.assertEqual(result["rows_per_publication"], 2000)
        self.assertEqual(result["measurement_boundary"], HARNESS.ONLINE_MEASUREMENT_BOUNDARY)
        self.assertTrue(all(value["status"] == "pass" for value in gates))

        evidence["transitions"][1]["cache"]["first_new_semantic_replay"] = True
        checks, _ = HARNESS.validate_online_transition(evidence["transitions"][1], "day1", "day2")
        self.assertFalse(checks["new_publication_misses_old_cache"])

    def test_online_contract_rejects_stable_but_wrong_old_oracle(self) -> None:
        evidence = online_value()
        transition = evidence["transitions"][0]
        transition["old_task"]["expected_publication_digest"] = digest("wrong-publication")
        transition["old_task"]["expected_result_sha256"] = digest("wrong-result")
        checks, _ = HARNESS.validate_online_transition(
            transition, "day0", "day1",
            evidence["fixture"]["publications"][0], evidence["fixture"]["publications"][1],
        )
        self.assertFalse(checks["old_task_returns_old_data"])

    def test_online_contract_rejects_arbitrary_matching_delegation_digest(self) -> None:
        evidence = online_value()
        transition = evidence["transitions"][0]
        arbitrary = digest("matching-but-not-target")
        transition["delegation"]["root_publication_digest"] = arbitrary
        transition["delegation"]["child_publication_digest"] = arbitrary
        checks, _ = HARNESS.validate_online_transition(
            transition, "day0", "day1",
            evidence["fixture"]["publications"][0], evidence["fixture"]["publications"][1],
        )
        self.assertFalse(checks["delegated_child_uses_root_publication"])

    def test_fixture_relation_keeps_online_scale_explicit(self) -> None:
        evidence = online_value()
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "online.json"
            path.write_text(json.dumps(evidence), encoding="utf-8")
            online, _ = HARNESS.online_evidence(path)
        self.assertEqual(
            HARNESS.classify_fixture_relation(345000, digest("offline-scale"), online),
            "separate_correctness_and_scale_fixtures",
        )
        self.assertEqual(
            HARNESS.classify_fixture_relation(2000, evidence["fixture"]["dataset_manifest_sha256"], online),
            "same_dataset_distinct_attested_artifacts",
        )
        self.assertEqual(
            HARNESS.classify_fixture_relation(2000, digest("other-dataset"), online),
            "same_scale_distinct_fixture",
        )

    def test_validate_online_cli_and_summary_exit_codes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            evidence_path = root / "online.json"
            evidence_path.write_text(json.dumps(online_value()), encoding="utf-8")
            self.assertEqual(HARNESS.main(["validate-online", "--evidence", str(evidence_path)]), 0)

            invalid = online_value()
            invalid["transitions"][0]["old_task"]["expected_result_sha256"] = digest("wrong")
            invalid_path = root / "invalid.json"
            invalid_path.write_text(json.dumps(invalid), encoding="utf-8")
            self.assertEqual(HARNESS.main(["validate-online", "--evidence", str(invalid_path)]), 1)

            for acceptance, expected in (("pass", 0), ("incomplete", 2), ("fail", 1)):
                output = root / f"{acceptance}.json"
                with mock.patch.object(HARNESS, "summarize", return_value={"acceptance": acceptance}):
                    actual = HARNESS.main([
                        "summarize", "--config", "unused", "--rows", "2000",
                        "--raw-dir", "unused", "--dataset-manifest", "unused",
                        "--output", str(output),
                    ])
                self.assertEqual(actual, expected)

    def test_provenance_includes_runner_and_copied_online_evidence(self) -> None:
        repository = pathlib.Path(__file__).resolve().parents[2]
        source = HARNESS.source_provenance(repository)
        self.assertIn("evaluation/cmd/rq5-online-transition/run.go", source["files"])
        self.assertIn("cmd/snapshot-sidecar-install/main.go", source["files"])
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "online-evidence.json").write_text("{}\n", encoding="utf-8")
            (root / "results.json").write_text("{}\n", encoding="utf-8")
            raw = HARNESS.raw_evidence_provenance(root)
        self.assertIn("online-evidence.json", raw["files"])
        self.assertNotIn("results.json", raw["files"])

    def test_pending_record_contains_no_measurement(self) -> None:
        value = HARNESS.pending_result(self.config_path)
        self.assertEqual(value["acceptance"], "incomplete")
        self.assertEqual(value["offline"]["status"], "not_measured")
        self.assertIsNone(value["online"]["latency_ms"])
        self.assertTrue(all(item["status"] == "unmeasured" for item in value["gates"]))


if __name__ == "__main__":
    unittest.main()
