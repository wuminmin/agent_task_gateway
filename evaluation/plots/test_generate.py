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

            metadata[0]["git_dirty"] = True
            (runs[0][0] / "run.json").write_text(json.dumps(metadata[0]) + "\n", encoding="utf-8")
            with self.assertRaises(SystemExit):
                generate.discover_runs(raw, [], False)
            self.assertEqual(generate.discover_runs(raw, [], True), [])

    def test_partial_full_campaign_is_never_complete(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            raw = pathlib.Path(temporary)
            metadata = self.metadata("full-partial", "taskgate-sf1-four-baseline", "full-sf1-partial")
            self.write_run(raw, metadata)
            with self.assertRaises(SystemExit):
                generate.discover_runs(raw, [], False)
            self.assertEqual(generate.discover_runs(raw, [], True), [])


if __name__ == "__main__":
    unittest.main()
