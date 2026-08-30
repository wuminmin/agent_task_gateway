"""Tests for the Final-V5 publication-campaign evidence validator (unittest, no pytest)."""

from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from final_v5_publication_evidence import (
    ATTACK_CELLS,
    ATTACK_CORPUS_RELATIVE_PATH,
    ATTACK_SEQUENCES,
    CAMPAIGN_EVIDENCE_RECORD,
    CONCURRENCY_CELLS,
    MANIFEST_RELATIVE_PATH,
    MANIFEST_VERSION,
    PHASE_CELLS,
    PIPELINE_PHASES,
    PROVSQL_BOUNDARY,
    PROVSQL_SCALES,
    PROVSQL_SYSTEMS,
    PROVSQL_WORKLOAD,
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


# Synthetic attack corpus mirroring the frozen A--E shapes: (step id, classification,
# role, task route, [actual R, D, O], [charged R, D, O] in the novel arm, error code).
_REJ = "SQL_NOT_LOWERABLE"
ATTACK_STEPS = {
    "A-pagination/complete-to-pages": [
        ("complete", "accepted", "complete", "root", (12, 18, 1), (12, 18, 1), None),
        ("page-1", "accepted", "partition", "root", (4, 6, 1), (0, 0, 1), None),
        ("page-overlap", "accepted", "overlap", "root", (4, 6, 1), (0, 0, 1), None),
        ("page-1-replay", "semantic_replay", "replay", "root", (4, 6, 1), (0, 0, 0), None),
    ],
    "A-pagination/pages-to-complete": [
        ("page-1", "accepted", "partition", "root", (4, 6, 1), (4, 6, 1), None),
        ("page-2", "accepted", "partition", "root", (8, 12, 1), (8, 12, 1), None),
        ("complete", "accepted", "complete", "root", (12, 18, 1), (0, 0, 1), None),
    ],
    "B-equivalent-sql/variants-v1": [
        ("equal-canonical", "accepted_equivalent", "equivalent", "root", (2, 3, 2), (2, 3, 2), None),
        ("equal-reversed", "accepted_equivalent", "equivalent", "root", (2, 3, 2), (0, 0, 0), None),
        ("unsupported-set-operation", "expected_rejection", "negative", "root", (0, 0, 0), (0, 0, 0), _REJ),
    ],
    "C-request-id/same-and-different": [
        ("request-id-control", "accepted", "request_identity", "root", (6, 9, 1), (6, 9, 1), None),
    ],
    "D-split-union/complete-to-split": [
        ("complete", "accepted", "complete", "root", (12, 18, 2), (12, 18, 2), None),
        ("split-low", "accepted", "partition", "root", (4, 6, 2), (0, 0, 2), None),
        ("split-high", "accepted", "partition", "root", (8, 12, 2), (0, 0, 2), None),
        ("public-union-negative", "expected_rejection", "negative", "root", (0, 0, 0), (0, 0, 0), _REJ),
    ],
    "D-split-union/split-to-complete": [
        ("split-low", "accepted", "partition", "root", (4, 6, 2), (4, 6, 2), None),
        ("split-high", "accepted", "partition", "root", (8, 12, 2), (8, 12, 2), None),
        ("complete", "accepted", "complete", "root", (12, 18, 2), (0, 0, 2), None),
        ("public-union-negative", "expected_rejection", "negative", "root", (0, 0, 0), (0, 0, 0), _REJ),
    ],
    "E-threshold/preregistered-v1": [
        ("threshold-300", "accepted", "threshold", "delegated_child", (1, 12, 2), (1, 12, 2), None),
        ("threshold-320", "accepted", "threshold", "root", (1, 12, 2), (0, 0, 2), None),
        ("outcome-primer-320-detail", "accepted", "outcome_primer", "root", (12, 18, 2), (12, 6, 1), None),
        ("threshold-880-budget", "expected_rejection", "threshold_rejection", "delegated_child", (0, 0, 0), (0, 0, 0), "EXPOSURE_BUDGET_EXHAUSTED"),
    ],
}


def _attack_corpus() -> dict:
    cases = []
    for sequence, steps in ATTACK_STEPS.items():
        workload, scale = sequence.split("/")
        case = {"workload_id": workload, "scale": scale, "steps": []}
        if workload == "E-threshold":
            case.update({"outcome_ceiling": 5, "thresholds": [300, 320, 880], "threshold_results": [6, 6, 4]})
        for step_id, classification, role, route, _, _, code in steps:
            step = {"id": step_id, "logical_sql": "SELECT 1", "direct_sql": "SELECT 1",
                    "classification": classification, "role": role}
            if route != "root":
                step["task_route"] = route
            if code:
                step["expected_error_code"] = code
            case["steps"].append(step)
        cases.append(case)
    return {"schema_version": 1, "corpus_id": "test-attack-corpus", "dataset_id": "test", "cases": cases}


def _attack_sample(cell: str, corpus_sha256: str) -> dict:
    sequence, mode = cell.rsplit("/", 1)
    novel = mode == "novel"
    steps = []
    for index, (step_id, classification, role, route, actual, charged, code) in enumerate(ATTACK_STEPS[sequence], 1):
        rejected = classification == "expected_rejection"
        observed = actual if mode != "direct" else (0, 0, 0)
        charge = charged if novel else (0, 0, 0)
        step = {"index": index, "variant_id": step_id, "classification": classification, "role": role,
                "accepted": not rejected, "rejected": rejected, "row_count": 0 if rejected else 6,
                "column_count": 0 if rejected else 2,
                "actual_release_facts": observed[0], "charged_release_facts": charge[0],
                "actual_dependency_facts": observed[1], "charged_dependency_facts": charge[1],
                "actual_outcome_facts": observed[2], "charged_outcome_facts": charge[2]}
        if role == "threshold":
            step["scalar_int64"] = 6
        if rejected:
            step["observed_error_code"] = code
            step["observed_error_reason"] = "ROOT_OUTCOME_CEILING_EXCEEDED" if code.startswith("EXPOSURE") else "SET_OPERATION_UNSUPPORTED"
        steps.append(step)
    verification = {"corpus_sha256": corpus_sha256, "steps": steps}
    if sequence.startswith("E-threshold"):
        verification.update({"expected_thresholds": [300, 320, 880], "observed_threshold_results": [6, 6],
                             "outcome_ceiling": 5, "observed_outcome": 5 if novel else 0})
    totals = [sum(step[key] for step in steps) for key in ("charged_release_facts", "charged_dependency_facts", "charged_outcome_facts")]
    return {
        "campaign_class": "publication", "record": SAMPLE_RECORD,
        "sample": {"campaign_id": CAMPAIGN_ID, "experiment_id": "attack", "cell_id": cell, "mode": mode,
                   "workload_id": sequence.split("/")[0], "scale": sequence.split("/")[1],
                   "warmup": False, "status": "pass", "publication_eligible": True,
                   "charged_release_facts": totals[0], "charged_dependency_facts": totals[1],
                   "charged_outcome_facts": totals[2], "attack_verification": verification},
    }


def _provsql_sample(scale: str, system: str, drain_ms: float) -> dict:
    facts = {"1k": 29003, "10k": 290003, "45k": 1305003}[scale]
    sample = {"campaign_id": CAMPAIGN_ID, "experiment_id": "provsql",
              "cell_id": f"{PROVSQL_WORKLOAD}/{scale}/{'direct' if system == 'postgresql' else system}",
              "workload_id": PROVSQL_WORKLOAD, "scale": scale, "system": system,
              "mode": "direct" if system == "postgresql" else system,
              "warmup": False, "status": "pass", "publication_eligible": True,
              "client_full_drain_ms": drain_ms, "row_count": 3,
              "actual_release_facts": 12 if system == "taskgate" else 0,
              "actual_dependency_facts": facts if system == "taskgate" else 0}
    if system == "provsql":
        sample["provsql_verification"] = {"boundary": PROVSQL_BOUNDARY}
    return {"campaign_class": "publication", "record": SAMPLE_RECORD, "sample": sample}


def _build_tree(root: Path, *, profiles=("analytics-orders", "expense-detail")) -> dict:
    campaign_root = root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID
    corpus_path = root / ATTACK_CORPUS_RELATIVE_PATH
    corpus_path.parent.mkdir(parents=True, exist_ok=True)
    corpus_path.write_text(json.dumps(_attack_corpus()))
    corpus_sha256 = _sha(corpus_path)
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
            attack_lines = [json.dumps(_attack_sample(cell, corpus_sha256)) for cell in ATTACK_CELLS for _ in range(3)]
            attack_raw = dep / "raw" / "attack.jsonl"
            attack_raw.write_text("\n".join(attack_lines) + "\n")
            provsql_lines = []
            for scale in PROVSQL_SCALES:
                for system in PROVSQL_SYSTEMS:
                    base = {"postgresql": 10.0, "provsql": 2000.0, "taskgate": 200.0}[system] * {"1k": 1, "10k": 2, "45k": 4}[scale]
                    for i in range(3):
                        provsql_lines.append(json.dumps(_provsql_sample(scale, system, base + i + repetition)))
            provsql_raw = dep / "raw" / "provsql.jsonl"
            provsql_raw.write_text("\n".join(provsql_lines) + "\n")
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
                           "sha256": _sha(concurrency_raw), "bytes": concurrency_raw.stat().st_size},
                          {"kind": "raw_jsonl", "experiment": "attack",
                           "path": f"deployments/{alias}/{repetition:03d}/raw/attack.jsonl",
                           "sha256": _sha(attack_raw), "bytes": attack_raw.stat().st_size},
                          {"kind": "raw_jsonl", "experiment": "provsql",
                           "path": f"deployments/{alias}/{repetition:03d}/raw/provsql.jsonl",
                           "sha256": _sha(provsql_raw), "bytes": provsql_raw.stat().st_size}],
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
        self.assertEqual(stats["measured_samples"],
                         6 * len(CELLS) * 4 * 3 + 6 * len(CONCURRENCY_CELLS) * 3 + 6 * len(ATTACK_CELLS) * 3 + 6 * 9 * 3)
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
        spread = stats["novel_spread"]
        self.assertEqual(set(spread), set(CELLS))
        for cell in CELLS:
            self.assertEqual(len(spread[cell]["execution_medians"]), 3)
            # the fixture adds the repetition number to every latency, so the medians rise by 1 per execution
            self.assertAlmostEqual(spread[cell]["execution_medians"][2] - spread[cell]["execution_medians"][0], 2.0)
            self.assertGreater(spread[cell]["spread"], 0)
        self.assertEqual(set(stats["direct_spread"]), set(CELLS))
        self.assertEqual(set(stats["concurrency_spread"]), set(CONCURRENCY_CELLS))
        self.assertEqual(stats["concurrency_spread"]["serial-control/1/serial"]["spread"], 0.0)
        provsql = stats["provsql"]
        self.assertEqual(provsql["samples"], 6 * 9 * 3)
        self.assertEqual(provsql["samples_per_cell"], 6 * 3)
        self.assertEqual(provsql["scales"]["45k"]["dependency_facts"], 1305003)
        self.assertGreater(provsql["scales"]["45k"]["provsql_over_taskgate"], 1)
        self.assertGreater(provsql["scales"]["1k"]["taskgate_over_postgresql"], 1)
        self.assertLessEqual(provsql["provsql_over_taskgate_min"], provsql["provsql_over_taskgate_max"])
        attack = stats["attack"]
        self.assertEqual(attack["samples"], 6 * len(ATTACK_CELLS) * 3)
        self.assertEqual(attack["samples_per_cell"], 6 * 3)
        self.assertEqual(set(attack["sequences"]), set(ATTACK_SEQUENCES))
        pages = attack["sequences"]["A-pagination/pages-to-complete"]
        self.assertEqual(pages["charged"], {"release": 12, "dependency": 18, "outcome": 3})
        self.assertEqual(pages["complete"], {"release": 12, "dependency": 18, "rows": 6})
        ladder = attack["sequences"]["E-threshold/preregistered-v1"]["threshold"]
        self.assertEqual(ladder["rejection_step"], 4)
        self.assertEqual(ladder["rejection_code"], "EXPOSURE_BUDGET_EXHAUSTED")
        self.assertEqual(ladder["outcome_ceiling"], 5)
        self.assertEqual(attack["sequences"]["E-threshold/preregistered-v1"]["steps"][0]["task_route"], "delegated_child")

    def _rewrite_raw(self, raw: Path, index: int, transform) -> None:
        lines = [json.loads(line) for line in raw.read_text().splitlines()]
        for line in lines:
            transform(line["sample"])
        raw.write_text("\n".join(json.dumps(line) for line in lines) + "\n")
        record_path = raw.parent.parent / "deployment-record.json"
        record = json.loads(record_path.read_text())
        record["files"][index]["sha256"] = _sha(raw)
        record["files"][index]["bytes"] = raw.stat().st_size
        record_path.write_text(json.dumps(record))

    def test_attack_charge_that_varies_between_samples_is_rejected(self):
        raw = self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "deployments/analytics-orders/001/raw/attack.jsonl"
        seen = []

        def perturb(sample):
            if sample["cell_id"] == "A-pagination/complete-to-pages/novel" and not seen:
                seen.append(True)
                sample["attack_verification"]["steps"][1]["charged_release_facts"] = 4
                sample["charged_release_facts"] += 4
        self._rewrite_raw(raw, 2, perturb)
        self._reseal()
        with self.assertRaisesRegex(PublicationEvidenceError, "distinct step outcomes"):
            validate_final_v5_publication_evidence(self.root)

    def test_attack_decomposition_that_overcharges_is_rejected(self):
        for dep in (self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "deployments").glob("*/[0-9][0-9][0-9]"):
            def overcharge(sample):
                if sample["cell_id"] == "D-split-union/complete-to-split/novel":
                    sample["attack_verification"]["steps"][1]["charged_release_facts"] = 4
                    sample["charged_release_facts"] += 4
            self._rewrite_raw(dep / "raw" / "attack.jsonl", 2, overcharge)
        self._reseal()
        with self.assertRaisesRegex(PublicationEvidenceError, "its complete query holds"):
            validate_final_v5_publication_evidence(self.root)

    def test_attack_corpus_drift_is_rejected(self):
        corpus_path = self.root / ATTACK_CORPUS_RELATIVE_PATH
        corpus_path.write_text(corpus_path.read_text() + "\n")
        with self.assertRaisesRegex(PublicationEvidenceError, "binds corpus"):
            validate_final_v5_publication_evidence(self.root)

    def test_provsql_without_its_declared_boundary_is_rejected(self):
        raw = self.root / "evaluation/final-v5-wsl2/raw" / CAMPAIGN_ID / "deployments/analytics-orders/001/raw/provsql.jsonl"

        def strip(sample):
            if sample["system"] == "provsql":
                sample["provsql_verification"]["boundary"] = "visible_only"
        self._rewrite_raw(raw, 3, strip)
        self._reseal()
        with self.assertRaisesRegex(PublicationEvidenceError, "boundaries"):
            validate_final_v5_publication_evidence(self.root)

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
