import hashlib
import json
import pathlib
import tempfile
import unittest

import generate


class CampaignAdmissionTests(unittest.TestCase):
    def metadata(self, campaign: str, suite: str, run_id: str) -> dict:
        return {
            "schema_version": 1,
            "run_id": run_id,
            "suite": suite,
            "mode": "full",
            "campaign_id": campaign,
            "status": "complete",
            "finished_at": "2026-07-22T01:02:03Z",
            "git_revision": "a" * 40,
            "git_dirty": False,
            "go_version": "go1.25.0",
            "goos": "linux",
            "goarch": "amd64",
            "baseline_order": list(generate.BASELINE_ORDER),
            "baseline_order_seed": 20260722,
            "ordering_strategy": "seeded_random",
            "cell_order": [
                {"order": 1, "experiment": "tpch_sf1", "baseline": "direct_postgresql", "concurrency": 1}
            ],
            "cache_strategy": "warm",
            "task_concurrency_mode": "distinct_task",
            "workload_lineage": "TPC-derived",
            "environment_manifest_path": "evaluation/environment/test.json",
            "environment_manifest_sha256": "b" * 64,
            "concurrency": [1, 8, 32],
        }

    def write_run(self, raw: pathlib.Path, metadata: dict) -> pathlib.Path:
        directory = raw / metadata["run_id"]
        directory.mkdir()
        (directory / "run.json").write_text(json.dumps(metadata) + "\n", encoding="utf-8")
        return directory

    def write_manifest(self, raw: pathlib.Path, campaign: str, runs: list[tuple[pathlib.Path, dict]]) -> None:
        entries = []
        for directory, metadata in runs:
            digest = hashlib.sha256((directory / "run.json").read_bytes()).hexdigest()
            entries.append({"run_id": metadata["run_id"], "suite": metadata["suite"], "run_json_sha256": digest})
        value = {
            "schema_version": 1,
            "campaign_id": campaign,
            "mode": "full",
            "status": "complete",
            "git_revision": "a" * 40,
            "git_dirty": False,
            "runs": entries,
        }
        (raw / f"campaign-{campaign}.json").write_text(json.dumps(value) + "\n", encoding="utf-8")

    def test_selects_only_exact_sealed_full_campaign(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            raw = pathlib.Path(temporary)
            campaign = "full-20260722T010203Z"
            metadata = [
                self.metadata(campaign, "taskgate-sf1-four-baseline", "full-sf1-test"),
                self.metadata(campaign, "taskgate-sf10-four-baseline", "full-sf10-test"),
            ]
            runs = [(self.write_run(raw, item), item) for item in metadata]
            self.write_manifest(raw, campaign, runs)
            selected = generate.discover_runs(raw, [], False)
            self.assertEqual(set(selected), {directory for directory, _ in runs})

            second_campaign = "full-20260722T020304Z"
            second_metadata = [
                self.metadata(second_campaign, "taskgate-sf1-four-baseline", "full-sf1-second"),
                self.metadata(second_campaign, "taskgate-sf10-four-baseline", "full-sf10-second"),
            ]
            second_runs = [(self.write_run(raw, item), item) for item in second_metadata]
            self.write_manifest(raw, second_campaign, second_runs)
            selected = generate.discover_runs(raw, [], False)
            self.assertEqual(set(selected), {directory for directory, _ in runs + second_runs})

            metadata[0]["git_dirty"] = True
            (runs[0][0] / "run.json").write_text(json.dumps(metadata[0]) + "\n", encoding="utf-8")
            selected = generate.discover_runs(raw, [], False)
            self.assertEqual(set(selected), {directory for directory, _ in second_runs})

    def test_partial_full_campaign_is_never_complete(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            raw = pathlib.Path(temporary)
            metadata = self.metadata("full-partial", "taskgate-sf1-four-baseline", "full-sf1-partial")
            self.write_run(raw, metadata)
            with self.assertRaises(SystemExit):
                generate.discover_runs(raw, [], False)
            self.assertEqual(generate.discover_runs(raw, [], True), [])


class SummaryTests(unittest.TestCase):
    def test_summary_is_per_query_and_withholds_small_sample_p99(self) -> None:
        samples = [
            {
                "experiment": "tpch_sf1",
                "family": "tpch",
                "scale_factor": 1,
                "baseline": "direct_postgresql",
                "concurrency": 1,
                "query_id": "orders",
                "latency_ms": 10.0,
                "database_ms": 8.0,
            },
            {
                "experiment": "tpch_sf1",
                "family": "tpch",
                "scale_factor": 1,
                "baseline": "direct_postgresql",
                "concurrency": 1,
                "query_id": "orders",
                "latency_ms": 20.0,
                "database_ms": 12.0,
            },
            {
                "experiment": "tpch_sf1",
                "family": "tpch",
                "scale_factor": 1,
                "baseline": "native_view",
                "concurrency": 1,
                "query_id": "orders",
                "latency_ms": 30.0,
                "database_ms": 25.0,
            },
            {
                "experiment": "tpch_sf1",
                "family": "tpch",
                "scale_factor": 1,
                "baseline": "native_view",
                "concurrency": 1,
                "query_id": "orders",
                "latency_ms": 30.0,
                "database_ms": 25.0,
            },
            {
                "experiment": "tpch_sf1",
                "family": "tpch",
                "scale_factor": 1,
                "baseline": "direct_postgresql",
                "concurrency": 1,
                "query_id": "customers",
                "latency_ms": 100.0,
                "database_ms": 90.0,
            },
        ]
        cells = [
            {
                "experiment": "tpch_sf1",
                "family": "tpch",
                "scale_factor": 1,
                "baseline": "direct_postgresql",
                "concurrency": 1,
                "throughput_qps": 3.0,
                "measurement_seconds": 1.0,
            },
            {
                "experiment": "tpch_sf1",
                "family": "tpch",
                "scale_factor": 1,
                "baseline": "native_view",
                "concurrency": 1,
                "throughput_qps": 2.0,
                "measurement_seconds": 1.0,
            },
        ]

        summary = generate.build_summary(samples, cells)
        by_key = {(row["baseline"], row["query_id"]): row for row in summary}
        self.assertEqual(set(by_key), {
            ("direct_postgresql", "orders"),
            ("native_view", "orders"),
            ("direct_postgresql", "customers"),
        })
        self.assertIsNone(by_key[("direct_postgresql", "orders")]["p99_ms"])
        self.assertFalse(by_key[("direct_postgresql", "orders")]["p99_reportable"])
        self.assertEqual(by_key[("native_view", "orders")]["p50_ratio_vs_direct"], 2.0)


if __name__ == "__main__":
    unittest.main()
