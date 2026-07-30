import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest

FINALIZE_PATH = pathlib.Path(__file__).with_name("finalize.py")
SPEC = importlib.util.spec_from_file_location("provenance_baseline_finalize", FINALIZE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("cannot load provenance baseline finalizer")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
finalize = MODULE.finalize
required_gates = MODULE.REQUIRED_GATES


def valid_evidence(root: pathlib.Path) -> tuple[pathlib.Path, pathlib.Path, str]:
    commit = "6" * 40
    config = root / "config.json"
    config.write_text(
        json.dumps(
            {
                "schema_version": 1,
                "campaign_id": "test-campaign",
                "data_cache_strategy": "warm",
                "circuit_strategy": "novel_nonce",
                "warmups": 1,
                "runs": 1,
                "order_seed": 7,
                "statement_timeout_ms": 1000,
                "expected_provsql_version": "1.11.0",
                "expected_provsql_commit": commit,
                "workloads": [
                    {
                        "id": "q",
                        "scale": 10,
                        "expected_rows": 1,
                        "provenance_carrier_columns": 1,
                    }
                ],
            }
        )
        + "\n",
        encoding="utf-8",
    )
    config_digest = hashlib.sha256(config.read_bytes()).hexdigest()
    report = root / "report.json"
    report.write_text(
        json.dumps(
            {
                "schema_version": 1,
                "status": "complete_measured_campaign",
                "comparison_boundary": {
                    "id": "query-result-plus-provenance-representation-generation-v2"
                },
                "campaign": {
                    "id": "test-campaign",
                    "data_cache_strategy": "warm",
                    "circuit_strategy": "novel_nonce",
                    "warmups_per_workload_and_system": 1,
                    "measured_runs_per_workload_and_system": 1,
                    "order_seed": 7,
                    "statement_timeout_ms": 1000,
                },
                "gates": [
                    {"id": gate_id, "status": "pass"}
                    for gate_id in sorted(required_gates)
                ],
                "systems": {
                    "provsql": {
                        "extension_version": "1.11.0",
                        "source_commit": commit,
                    }
                },
                "dataset": {
                    "equal": True,
                    "fingerprint_rows": 1,
                    "direct_sha256": "d" * 64,
                    "provsql_sha256": "d" * 64,
                },
                "samples": [
                    {
                        "system": "direct_postgresql",
                        "workload_id": "q",
                        "scale": 10,
                        "iteration": 0,
                        "rows": 1,
                        "duration_ms": 1.0,
                        "result_sha256": "a" * 64,
                    },
                    {
                        "system": "provsql",
                        "workload_id": "q",
                        "scale": 10,
                        "iteration": 0,
                        "rows": 1,
                        "duration_ms": 2.0,
                        "result_sha256": "a" * 64,
                        "aggregate_tokens": 1,
                        "row_tokens": 1,
                        "provenance_representation_fields": 2,
                        "root_types_verified": True,
                        "provenance_representation_sha256": "b" * 64,
                        "gates_before": 10,
                        "gates_after": 12,
                        "gate_delta": 2,
                        "artifact_bytes_before": 100,
                        "artifact_bytes_after": 108,
                        "artifact_byte_delta": 8,
                    },
                ],
                "summaries": [
                    {
                        "workload_id": "q",
                        "scale": 10,
                        "system": "direct_postgresql",
                        "samples": 1,
                        "duration_ms": {
                            "count": 1,
                            "min": 1.0,
                            "p50": 1.0,
                            "p95": 1.0,
                            "max": 1.0,
                            "mean": 1.0,
                        },
                    },
                    {
                        "workload_id": "q",
                        "scale": 10,
                        "system": "provsql",
                        "samples": 1,
                        "duration_ms": {
                            "count": 1,
                            "min": 2.0,
                            "p50": 2.0,
                            "p95": 2.0,
                            "max": 2.0,
                            "mean": 2.0,
                        },
                        "gate_delta": {
                            "count": 1,
                            "min": 2.0,
                            "p50": 2.0,
                            "p95": 2.0,
                            "max": 2.0,
                            "mean": 2.0,
                        },
                        "artifact_byte_delta": {
                            "count": 1,
                            "min": 8.0,
                            "p50": 8.0,
                            "p95": 8.0,
                            "max": 8.0,
                            "mean": 8.0,
                        },
                    },
                ],
                "provenance": {
                    "config_sha256": config_digest,
                    "executable_sha256": "e" * 64,
                },
            }
        ),
        encoding="utf-8",
    )
    return report, config, commit


class FinalizeTest(unittest.TestCase):
    def test_binds_images_memory_config_and_report(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            report, config, commit = valid_evidence(root)
            output = root / "results.json"
            result = finalize(
                report,
                config,
                output,
                "sha256:" + "1" * 64,
                "sha256:" + "2" * 64,
                commit,
                "1024",
                "2048",
            )
            self.assertEqual(2, result["schema_version"])
            self.assertEqual(2048, result["container_evidence"]["memory"]["provsql_peak_bytes"])
            self.assertEqual("config.json", result["provenance"]["preserved_config_path"])
            with self.assertRaises(FileExistsError):
                finalize(
                    report,
                    config,
                    output,
                    "sha256:" + "1" * 64,
                    "sha256:" + "2" * 64,
                    commit,
                    "1024",
                    "2048",
                )

    def test_rejects_failed_or_missing_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            report_path, config, commit = valid_evidence(root)
            report = json.loads(report_path.read_text(encoding="utf-8"))
            report["gates"][0]["status"] = "fail"
            report_path.write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "failed gate"):
                finalize(report_path, config, root / "failed.json", "1" * 64, "2" * 64, commit, "1", "1")

            report["gates"] = report["gates"][1:]
            report_path.write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "required gate set"):
                finalize(report_path, config, root / "missing.json", "1" * 64, "2" * 64, commit, "1", "1")

    def test_rejects_config_digest_mismatch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            report, config, commit = valid_evidence(root)
            config.write_bytes(config.read_bytes() + b" ")
            with self.assertRaisesRegex(ValueError, "config bytes"):
                finalize(report, config, root / "out.json", "1" * 64, "2" * 64, commit, "1", "1")

    def test_rejects_incomplete_pairs_and_invalid_provenance_counts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            report_path, config, commit = valid_evidence(root)
            report = json.loads(report_path.read_text(encoding="utf-8"))
            report["samples"].pop()
            report_path.write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "samples or summaries"):
                finalize(report_path, config, root / "incomplete.json", "1" * 64, "2" * 64, commit, "1", "1")

            report_path, config, commit = valid_evidence(root)
            report = json.loads(report_path.read_text(encoding="utf-8"))
            report["samples"][1]["row_tokens"] = 0
            report_path.write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "verified aggregate roots"):
                finalize(report_path, config, root / "counts.json", "1" * 64, "2" * 64, commit, "1", "1")

    def test_rejects_tampered_summary_and_system_binding(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            report_path, config, commit = valid_evidence(root)
            report = json.loads(report_path.read_text(encoding="utf-8"))
            report["summaries"][0]["duration_ms"]["mean"] = 99.0
            report_path.write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "distribution differs"):
                finalize(report_path, config, root / "summary.json", "1" * 64, "2" * 64, commit, "1", "1")

            report_path, config, commit = valid_evidence(root)
            report = json.loads(report_path.read_text(encoding="utf-8"))
            report["systems"]["provsql"]["extension_version"] = "wrong"
            report_path.write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "system evidence"):
                finalize(report_path, config, root / "system.json", "1" * 64, "2" * 64, commit, "1", "1")


if __name__ == "__main__":
    unittest.main()
