"""Tests for the Final-V5 publication-campaign evidence validator (unittest, no pytest)."""

from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from final_v5_publication_evidence import (
    CAMPAIGN_EVIDENCE_RECORD,
    CONCURRENCY_CELLS,
    MANIFEST_RELATIVE_PATH,
    MANIFEST_VERSION,
    PHASE_CELLS,
    PIPELINE_PHASES,
    SAMPLE_RECORD,
    PublicationEvidenceError,
    validate_final_v5_publication_evidence,
)

CAMPAIGN_ID = "formal-test-01"
COMMIT = "a" * 40
CELLS = ("S1/SF1", "S1/SF10", "S2/SF1", "S2/SF10", "S6/100k-x16")
MODES = ("direct", "novel", "semantic_replay", "idempotent_replay")


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _pipeline(mode: str, drain_ms: float) -> dict:
    if mode != "novel":
        return {"prepare": 0.0, "execute_and_derive": drain_ms, "artifact_stage": 0.0,
                "control_settlement": 0.0, "artifact_publication": 0.0, "response_finalize": 0.0,
                "server_total": drain_ms}
    # Six non-overlapping Gateway phases that sum exactly to server_total.
    phases = {"prepare": 5.0, "execute_and_derive": drain_ms - 45.0, "artifact_stage": 20.0,
              "control_settlement": 12.0, "artifact_publication": 6.0, "response_finalize": 2.0}
    phases["server_total"] = sum(phases.values())
    return phases


def _concurrency_sample(cell: str, drain_ms: float) -> dict:
    return {
        "campaign_class": "publication",
        "record": SAMPLE_RECORD,
        "sample": {
            "campaign_id": CAMPAIGN_ID, "experiment_id": "concurrency", "cell_id": cell,
            "mode": cell.split("/")[2], "warmup": False, "status": "pass", "publication_eligible": True,
            "client_full_drain_ms": drain_ms,
        },
    }


def _sample(cell: str, mode: str, drain_ms: float, seed: int) -> dict:
    novel = mode == "novel"
    replay = mode in ("semantic_replay", "idempotent_replay")
    return {
        "campaign_class": "publication",
        "record": SAMPLE_RECORD,
        "sample": {
            "campaign_id": CAMPAIGN_ID, "experiment_id": "baseline", "cell_id": cell + "/" + mode,
            "mode": mode, "warmup": False, "status": "pass", "publication_eligible": True,
            "client_full_drain_ms": drain_ms,
            "pipeline_ms": _pipeline(mode, drain_ms),
            "business_sql_delta": 0 if replay else 1,
            "actual_release_facts": 100 if cell.startswith("S1") else 10,
            "actual_dependency_facts": 10 if cell.startswith("S1") else 1000,
            "row_count": 5,
            "charged_release_facts": 0 if replay else 100,
            "charged_dependency_facts": 0 if replay else 10,
            "charged_outcome_facts": 0 if replay else 1,
        },
    }


def _build_tree(root: Path, *, profiles=("analytics-orders", "expense-detail")) -> dict:
    campaign_root = root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID
    record_digests = []
    for alias in profiles:
        for repetition in (1, 2, 3):
            dep = campaign_root / "deployments" / alias / f"{repetition:03d}"
            (dep / "raw").mkdir(parents=True)
            lines = []
            for cell in CELLS:
                for mode in MODES:
                    base = 100.0 if mode == "direct" else 250.0 if mode == "novel" else 40.0
                    for i in range(3):  # three measured samples per cell/mode
                        lines.append(json.dumps(_sample(cell, mode, base + i + repetition, i)))
                    warm = _sample(cell, mode, 999.0, 0); warm["sample"]["warmup"] = True
                    lines.append(json.dumps(warm))
            raw = dep / "raw" / "baseline.jsonl"
            raw.write_text("\n".join(lines) + "\n")
            concurrency_lines = []
            for cell in CONCURRENCY_CELLS:
                width = int(cell.split("/")[1])
                for i in range(3):  # three measured samples per concurrency cell
                    concurrency_lines.append(json.dumps(_concurrency_sample(cell, 30.0 + 2.0 * width + i)))
            concurrency_raw = dep / "raw" / "concurrency.jsonl"
            concurrency_raw.write_text("\n".join(concurrency_lines) + "\n")
            record = {
                "schema_version": 1, "campaign_class": "publication", "publication_eligible": True,
                "formal_campaign": True, "campaign_id": CAMPAIGN_ID, "submission_commit": COMMIT,
                "repetition": repetition, "profile_alias": alias, "profile_id": "profile-" + alias,
                "cells": [alias + "/" + c for c in CELLS],
                "files": [{"kind": "raw_jsonl", "experiment": "baseline",
                           "path": f"deployments/{alias}/{repetition:03d}/raw/baseline.jsonl",
                           "sha256": _sha(raw), "bytes": raw.stat().st_size},
                          {"kind": "raw_jsonl", "experiment": "concurrency",
                           "path": f"deployments/{alias}/{repetition:03d}/raw/concurrency.jsonl",
                           "sha256": _sha(concurrency_raw), "bytes": concurrency_raw.stat().st_size}],
            }
            path = dep / "deployment-record.json"
            path.write_text(json.dumps(record))
            record_digests.append(_sha(path))
    sealed = campaign_root / "non-profile" / "compiler" / "evidence" / "manifest.json"
    sealed.parent.mkdir(parents=True)
    sealed.write_text('{"campaign_id": "%s"}' % CAMPAIGN_ID)
    profile_cells = len(profiles) * len(CELLS)
    evidence = {
        "schema_version": 1, "record": CAMPAIGN_EVIDENCE_RECORD, "status": "pass",
        "campaign_class": "publication", "publication_eligible": True, "formal_campaign": True,
        "campaign_id": CAMPAIGN_ID, "submission_commit": COMMIT, "plan_sha256": "0" * 64,
        "profile_cells": profile_cells, "scale_non_profile_cells": 2, "compiler_non_profile_cells": 1,
        "total_cells": profile_cells + 3,
        "profile_campaign": {"execution_model": "fresh_profile_deployment", "cells": profile_cells,
                             "fresh_executions": 3, "profile_binding": "required",
                             "state_inheritance": False, "evidence_sha256": sorted(record_digests)},
        "non_profile_campaigns": {"compiler": {"execution_model": "fresh_process", "cells": 1,
                                               "fresh_executions": 3, "profile_binding": "forbidden",
                                               "state_inheritance": False, "evidence_sha256": [_sha(sealed)]}},
    }
    evidence_path = campaign_root / "campaign-evidence.json"
    evidence_path.write_text(json.dumps(evidence))
    manifest = {
        "version": MANIFEST_VERSION, "publication_eligible": True,
        "campaign": {"id": CAMPAIGN_ID, "path": f"evaluation/final-v5-wsl2/raw/{CAMPAIGN_ID}",
                     "campaign_evidence_sha256": _sha(evidence_path), "submission_commit": COMMIT,
                     "deployments": len(profiles) * 3, "profile_cells": profile_cells,
                     "scale_non_profile_cells": 2, "compiler_non_profile_cells": 1,
                     "total_cells": profile_cells + 3},
    }
    (root / MANIFEST_RELATIVE_PATH).parent.mkdir(parents=True, exist_ok=True)
    (root / MANIFEST_RELATIVE_PATH).write_text(json.dumps(manifest))
    return manifest


class PublicationEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        _build_tree(self.root)

    def tearDown(self):
        self.tmp.cleanup()

    def test_synthetic_campaign_yields_the_cited_statistics(self):
        stats = validate_final_v5_publication_evidence(self.root)
        self.assertEqual(stats["deployments"], 6)
        self.assertEqual(stats["profiles"], 2)
        self.assertEqual(stats["profile_cells"], 2 * len(CELLS))
        self.assertEqual(stats["total_cells"], 2 * len(CELLS) + 3)
        self.assertEqual(stats["fresh_executions"], 3)
        self.assertEqual(stats["measured_samples"], 6 * len(CELLS) * 4 * 3 + 6 * len(CONCURRENCY_CELLS) * 3)
        baseline = stats["baseline"]
        self.assertEqual(baseline["replay_zero_sql_samples"], 6 * len(CELLS) * 2 * 3)
        self.assertGreater(baseline["overhead_min"], 1)
        for cell in CELLS:
            self.assertIn(cell, baseline["cell_medians"])
            self.assertLess(baseline["cell_medians"][cell]["idempotent_ms"], baseline["cell_medians"][cell]["direct_ms"])
        self.assertEqual(stats["non_profile"]["compiler"]["cells"], 1)
        phases = stats["phases"]
        for cell in PHASE_CELLS:
            self.assertIn(cell, phases)
            self.assertEqual(phases[cell]["dominant_phase"], "execute_and_derive")
            self.assertAlmostEqual(
                sum(phases[cell][key] for key in PIPELINE_PHASES), phases[cell]["server_total"], places=6)
            self.assertEqual(phases[cell]["samples"], 6 * 3)
        concurrency = stats["concurrency"]
        self.assertEqual(set(concurrency), set(CONCURRENCY_CELLS))
        fifty = concurrency["shared-root/50/natural_contention"]
        self.assertEqual(fifty["width"], 50)
        self.assertEqual(fifty["rounds"], 6 * 3)
        self.assertGreater(fifty["drain_p50_ms"], concurrency["serial-control/1/serial"]["drain_p50_ms"])
        self.assertLessEqual(fifty["drain_p50_ms"], fifty["drain_p95_ms"])
        self.assertAlmostEqual(fifty["requests_per_second_at_p50"], 50 * 1000.0 / fifty["drain_p50_ms"])

    def test_missing_pipeline_phase_is_rejected(self):
        raw = self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "deployments/analytics-orders/001/raw/baseline.jsonl"
        lines = [json.loads(line) for line in raw.read_text().splitlines()]
        for line in lines:
            if line["sample"]["mode"] == "novel":
                del line["sample"]["pipeline_ms"]["control_settlement"]
        raw.write_text("\n".join(json.dumps(line) for line in lines) + "\n")
        record_path = raw.parent.parent / "deployment-record.json"
        record = json.loads(record_path.read_text())
        record["files"][0]["sha256"] = _sha(raw)
        record["files"][0]["bytes"] = raw.stat().st_size
        record_path.write_text(json.dumps(record))
        self._reseal()
        with self.assertRaisesRegex(PublicationEvidenceError, "pipeline phases"):
            validate_final_v5_publication_evidence(self.root)

    def test_missing_concurrency_cell_is_rejected(self):
        for dep in (self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "deployments").glob("*/[0-9][0-9][0-9]"):
            raw = dep / "raw" / "concurrency.jsonl"
            lines = [line for line in raw.read_text().splitlines() if "serial-control" not in line]
            raw.write_text("\n".join(lines) + "\n")
            record_path = dep / "deployment-record.json"
            record = json.loads(record_path.read_text())
            record["files"][1]["sha256"] = _sha(raw)
            record["files"][1]["bytes"] = raw.stat().st_size
            record_path.write_text(json.dumps(record))
        self._reseal()
        with self.assertRaisesRegex(PublicationEvidenceError, "missing cells"):
            validate_final_v5_publication_evidence(self.root)

    def _reseal(self):
        """Re-seal deployment-record digests into the campaign evidence and manifest."""
        campaign_root = self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID
        digests = sorted(_sha(path) for path in campaign_root.glob("deployments/*/[0-9][0-9][0-9]/deployment-record.json"))
        evidence_path = campaign_root / "campaign-evidence.json"
        evidence = json.loads(evidence_path.read_text())
        evidence["profile_campaign"]["evidence_sha256"] = digests
        evidence_path.write_text(json.dumps(evidence))
        manifest_path = self.root / MANIFEST_RELATIVE_PATH
        manifest = json.loads(manifest_path.read_text())
        manifest["campaign"]["campaign_evidence_sha256"] = _sha(evidence_path)
        manifest_path.write_text(json.dumps(manifest))

    def test_tampered_sample_file_is_rejected(self):
        raw = self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "deployments/analytics-orders/001/raw/baseline.jsonl"
        raw.write_text(raw.read_text() + "\n")
        with self.assertRaisesRegex(PublicationEvidenceError, "changed since the campaign sealed it"):
            validate_final_v5_publication_evidence(self.root)

    def test_pilot_class_evidence_is_rejected(self):
        path = self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "campaign-evidence.json"
        evidence = json.loads(path.read_text()); evidence["campaign_class"] = "pilot"
        path.write_text(json.dumps(evidence))
        manifest_path = self.root / MANIFEST_RELATIVE_PATH
        manifest = json.loads(manifest_path.read_text())
        manifest["campaign"]["campaign_evidence_sha256"] = _sha(path)
        manifest_path.write_text(json.dumps(manifest))
        with self.assertRaisesRegex(PublicationEvidenceError, "campaign_class"):
            validate_final_v5_publication_evidence(self.root)

    def test_missing_repetition_is_rejected(self):
        import shutil
        shutil.rmtree(self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "deployments/expense-detail/003")
        with self.assertRaisesRegex(PublicationEvidenceError, "deployment records"):
            validate_final_v5_publication_evidence(self.root)

    def test_stale_campaign_evidence_digest_is_rejected(self):
        manifest_path = self.root / MANIFEST_RELATIVE_PATH
        manifest = json.loads(manifest_path.read_text())
        manifest["campaign"]["campaign_evidence_sha256"] = "f" * 64
        manifest_path.write_text(json.dumps(manifest))
        with self.assertRaisesRegex(PublicationEvidenceError, "digest"):
            validate_final_v5_publication_evidence(self.root)


if __name__ == "__main__":
    unittest.main()
