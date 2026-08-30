#!/usr/bin/env python3
"""Validate the retained Final-V5 pilot runs the manuscript is allowed to cite.

These runs are ``campaign_class=pilot`` and ``publication_eligible=false``. That
is not a reason to leave their numbers out of the paper -- they are real
measurements of a real deployment -- but it is a reason the paper must say what
class they are, and a reason nothing here may be described as a completed
publication campaign.

Every number this module returns is derived from the retained sample bytes named
by ``evaluation/final-v5-wsl2/pilot-evidence-v1.json`` and checked against that
manifest's digest, so a paper macro can never quietly move onto a rerun.
"""

from __future__ import annotations

import hashlib
import json
import statistics
from pathlib import Path

MANIFEST_RELATIVE_PATH = "evaluation/final-v5-wsl2/pilot-evidence-v1.json"
MANIFEST_VERSION = "taskgate-final-v5-pilot-evidence-v1"

# The Baseline coordinates the manuscript names directly. Every other cell the
# retained run carries is still summarised, but these four must be present
# because the paper cites them by name.
BASELINE_CELLS = ("S1/SF1", "S1/SF10", "S2/SF1", "S2/SF10")
REPLAY_MODES = ("semantic_replay", "normalized_rewrite_replay", "idempotent_replay")


class PilotEvidenceError(RuntimeError):
    """Raised when retained pilot evidence does not support a paper macro."""


def _read_samples(path: Path) -> list[dict]:
    payload = path.read_bytes()
    samples = []
    for line in payload.decode("utf-8").splitlines():
        line = line.strip()
        if line:
            samples.append(json.loads(line))
    return samples


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _measured(samples: list[dict]) -> list[dict]:
    return [sample for sample in samples if not sample.get("warmup")]


def _median(values: list[float]) -> float:
    if not values:
        raise PilotEvidenceError("cannot take the median of an empty sample set")
    return statistics.median(values)


def _load_run(root: Path, entry: dict) -> list[dict]:
    run_dir = root / entry["path"]
    samples_path = run_dir / "raw" / "deployment-01.jsonl"
    if not samples_path.is_file():
        raise PilotEvidenceError(f"retained pilot run {entry['id']} has no sample file")
    observed = _digest(samples_path)
    if observed != entry["samples_sha256"]:
        raise PilotEvidenceError(
            f"retained pilot run {entry['id']} samples digest {observed[:12]} "
            f"differs from the manifest's {entry['samples_sha256'][:12]}"
        )
    config = json.loads((run_dir / "config.json").read_text())
    if config.get("campaign_class") != "pilot" or config.get("pilot_kind") != entry["pilot_kind"]:
        raise PilotEvidenceError(f"retained pilot run {entry['id']} is not the declared pilot class")
    if config.get("submission_commit") != entry["submission_commit"]:
        raise PilotEvidenceError(f"retained pilot run {entry['id']} was not run from the manifest's commit")
    # A finalized run carries a summary; the targeted artifact launcher runs its
    # own acceptance instead and writes no summary. Both must carry the
    # not-for-publication marker, and in both the samples themselves are what
    # actually gates a paper macro.
    summary_path = run_dir / "generated" / "summary.json"
    if summary_path.is_file():
        summary = json.loads(summary_path.read_text())
        if summary.get("publication_eligible") is not False:
            raise PilotEvidenceError(f"retained pilot run {entry['id']} claims publication eligibility")
        if summary.get("status") != "pass":
            raise PilotEvidenceError(f"retained pilot run {entry['id']} did not finalize as pass")
    if not any(run_dir.glob("*NOT-FOR-PUBLICATION")):
        raise PilotEvidenceError(f"retained pilot run {entry['id']} has no not-for-publication marker")
    samples = _measured(_read_samples(samples_path))
    claimed = [sample for sample in samples if sample.get("publication_eligible") is not False]
    if claimed:
        raise PilotEvidenceError(f"retained pilot run {entry['id']} has {len(claimed)} publication-eligible samples")
    if len(samples) != entry["measured_samples"]:
        raise PilotEvidenceError(
            f"retained pilot run {entry['id']} has {len(samples)} measured samples, "
            f"manifest declares {entry['measured_samples']}"
        )
    failed = [sample for sample in samples if sample.get("status") != "pass"]
    if failed:
        raise PilotEvidenceError(f"retained pilot run {entry['id']} retains {len(failed)} non-passing samples")
    return samples


# Gateway component timings retained by the sub-phase pilot (profile-campaign
# pilot with the adapter keeping the Gateway's component_ms map).
SUBPHASE_CELLS = ("S1/SF1", "S2/SF10", "S6/100k-x16")
SUBPHASE_COMPONENTS = (
    "business_postgresql", "provenance_postgresql", "ordinal_visible_preparation", "ordinal_stream",
    "ordinal_stream_consumer", "ordinal_finish", "bitmap_derivation", "exposure_derivation",
    "result_encoding", "encryption", "settle_persist", "exposure_fact_store", "receipt_signing",
)


def _load_profile_pilot(root: Path, entry: dict) -> list[dict]:
    """Load a profile-campaign pilot run (deployments/<alias>/NNN/raw/*.jsonl)."""
    run_dir = root / entry["path"]
    evidence_path = run_dir / "campaign-evidence.json"
    if not evidence_path.is_file():
        raise PilotEvidenceError(f"pilot campaign {entry['id']} has no campaign-evidence.json")
    observed = _digest(evidence_path)
    if observed != entry["campaign_evidence_sha256"]:
        raise PilotEvidenceError(f"pilot campaign {entry['id']} evidence digest {observed[:12]} differs from the manifest's")
    evidence = json.loads(evidence_path.read_text())
    if evidence.get("campaign_class") != "pilot" or evidence.get("publication_eligible") is not False:
        raise PilotEvidenceError(f"pilot campaign {entry['id']} is not a publication-ineligible pilot")
    if evidence.get("submission_commit") != entry["submission_commit"]:
        raise PilotEvidenceError(f"pilot campaign {entry['id']} was not run from the manifest's commit")
    samples = []
    for record_path in sorted(run_dir.glob("deployments/*/[0-9][0-9][0-9]/deployment-record.json")):
        record = json.loads(record_path.read_text())
        if record.get("campaign_class") != "pilot" or record.get("publication_eligible") is not False:
            raise PilotEvidenceError(f"pilot campaign {entry['id']} deployment record is not pilot class")
        for file in record.get("files", []):
            if file.get("kind") != "raw_jsonl":
                continue
            path = run_dir / file["path"]
            if not path.is_file() or path.is_symlink() or _digest(path) != file["sha256"]:
                raise PilotEvidenceError(f"pilot campaign {entry['id']} sample file {file['path']} is absent or changed")
            for line in _read_samples(path):
                sample = line.get("sample", {})
                if sample.get("warmup"):
                    continue
                if line.get("campaign_class") != "pilot" or sample.get("publication_eligible") is not False:
                    raise PilotEvidenceError(f"pilot campaign {entry['id']} carries a non-pilot sample")
                if sample.get("status") != "pass":
                    raise PilotEvidenceError(f"pilot campaign {entry['id']} retains a non-passing sample")
                samples.append(sample)
    if len(samples) != entry["measured_samples"]:
        raise PilotEvidenceError(f"pilot campaign {entry['id']} has {len(samples)} measured samples, manifest declares {entry['measured_samples']}")
    return samples


def _subphase_stats(samples: list[dict]) -> dict:
    """Per-component medians of the novel arm for the cells the paper discusses."""
    by_cell: dict[str, list[dict]] = {}
    for sample in samples:
        if sample.get("experiment_id") != "baseline" or sample.get("mode") != "novel":
            continue
        cell = "/".join(sample["cell_id"].split("/")[:2])
        by_cell.setdefault(cell, []).append(sample)
    missing = [cell for cell in SUBPHASE_CELLS if cell not in by_cell]
    if missing:
        raise PilotEvidenceError(f"sub-phase pilot is missing novel cells {missing}")
    result = {}
    for cell in SUBPHASE_CELLS:
        values = by_cell[cell]
        components: dict[str, list[float]] = {}
        for sample in values:
            component = sample.get("component_ms")
            if not component:
                raise PilotEvidenceError(f"sub-phase pilot sample in {cell} retains no component_ms")
            for name, value in component.items():
                components.setdefault(name, []).append(float(value))
        execute = _median([float(s["pipeline_ms"]["execute_and_derive"]) for s in values])
        medians = {name: _median(vals) for name, vals in components.items()}
        result[cell] = {
            "samples": len(values),
            "execute_and_derive_p50_ms": execute,
            "server_total_p50_ms": _median([float(s["pipeline_ms"]["server_total"]) for s in values]),
            "components_p50_ms": medians,
            "dependency_facts": values[0]["actual_dependency_facts"],
            "release_facts": values[0]["actual_release_facts"],
        }
    return result


def validate_final_v5_pilot_evidence(root: Path) -> dict:
    """Return the statistics the manuscript cites from retained pilot runs."""
    manifest_path = root / MANIFEST_RELATIVE_PATH
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("version") != MANIFEST_VERSION:
        raise PilotEvidenceError("pilot evidence manifest version is not the expected one")
    if manifest.get("publication_eligible") is not False:
        raise PilotEvidenceError("pilot evidence manifest must declare itself publication-ineligible")

    artifact_runs, baseline_samples, subphase_samples = [], [], []
    for entry in manifest["runs"]:
        if entry["family"] == "subphase":
            subphase_samples.extend(_load_profile_pilot(root, entry))
            continue
        samples = _load_run(root, entry)
        if entry["family"] == "artifact":
            artifact_runs.append((entry, samples))
        elif entry["family"] == "baseline":
            baseline_samples.extend(samples)
        else:
            raise PilotEvidenceError(f"pilot evidence manifest names unknown family {entry['family']!r}")

    if not artifact_runs or not baseline_samples:
        raise PilotEvidenceError("pilot evidence manifest must name both an artifact and a baseline run")

    # The artifact runs are the independence claim: separate deployments of one
    # commit, not one deployment measured repeatedly.
    commits = {entry["submission_commit"] for entry, _ in artifact_runs}
    if len(commits) != 1:
        raise PilotEvidenceError("artifact pilot deployments do not share one submission commit")
    artifact_cells = {sample["cell_id"] for _, samples in artifact_runs for sample in samples}
    artifact_stats = {
        "deployments": len(artifact_runs),
        "commit": commits.pop(),
        "cells": len(artifact_cells),
        "samples": sum(len(samples) for _, samples in artifact_runs),
        "samples_per_cell": sum(len(samples) for _, samples in artifact_runs) // len(artifact_cells),
    }

    by_cell: dict[str, dict[str, list[float]]] = {}
    exposure: dict[str, dict[str, int]] = {}
    for sample in baseline_samples:
        workload_scale = "/".join(sample["cell_id"].split("/")[:2])
        by_cell.setdefault(workload_scale, {}).setdefault(sample["mode"], []).append(
            sample["client_full_drain_ms"]
        )
        if sample["mode"] == "novel":
            exposure[workload_scale] = {
                "release": sample["actual_release_facts"],
                "dependency": sample["actual_dependency_facts"],
                "rows": sample["row_count"],
            }
    missing = [cell for cell in BASELINE_CELLS if cell not in by_cell]
    if missing:
        raise PilotEvidenceError(f"baseline pilot evidence is missing cells {missing}")

    # Zero business SQL on replay is the claim the manuscript makes about
    # replay, so it is asserted here rather than merely reported.
    replay_samples = [sample for sample in baseline_samples if sample["mode"] in REPLAY_MODES]
    replay_deltas = {sample["business_sql_delta"] for sample in replay_samples}
    if replay_deltas != {0}:
        raise PilotEvidenceError(f"baseline replay samples recorded business SQL deltas {sorted(replay_deltas)}")
    novel_deltas = {sample["business_sql_delta"] for sample in baseline_samples if sample["mode"] == "novel"}
    if not novel_deltas or min(novel_deltas) < 1:
        raise PilotEvidenceError(f"baseline novel samples recorded business SQL deltas {sorted(novel_deltas)}")
    # Semantic and normalized-rewrite replays settle all three Fact budgets at
    # zero while recording the same actual sets. Idempotent replay returns the
    # original record verbatim, so it is deliberately excluded here and its
    # no-recharge property is carried by the control-plane snapshot instead.
    for sample in baseline_samples:
        if sample["mode"] not in ("semantic_replay", "normalized_rewrite_replay"):
            continue
        charged = (
            sample["charged_release_facts"],
            sample["charged_dependency_facts"],
            sample["charged_outcome_facts"],
        )
        if charged != (0, 0, 0):
            raise PilotEvidenceError(f"baseline replay {sample['cell_id']} charged Facts {charged}")

    # Summarise every workload/scale the run carries, not only the four the
    # manuscript names. A run that grows new cells should widen the evidence
    # rather than be silently reduced to the old four.
    ratios = {}
    for cell in sorted(by_cell):
        modes = by_cell[cell]
        for mode in ("direct", "novel"):
            if mode not in modes:
                raise PilotEvidenceError(f"baseline cell {cell} has no {mode} samples")
        ratios[cell] = {
            "direct_ms": _median(modes["direct"]),
            "novel_ms": _median(modes["novel"]),
        }
        # S5 declares no rewrite mode and S6 declares no replays at all, so a
        # replay median is recorded when the cell has one rather than required.
        for mode, key in (("semantic_replay", "semantic_ms"), ("idempotent_replay", "idempotent_ms")):
            if mode in modes:
                ratios[cell][key] = _median(modes[mode])
        ratios[cell]["novel_over_direct"] = ratios[cell]["novel_ms"] / ratios[cell]["direct_ms"]

    overheads = [value["novel_over_direct"] for value in ratios.values()]
    baseline_cells = {sample["cell_id"] for sample in baseline_samples}
    baseline_stats = {
        "cells": len(baseline_cells),
        "samples": len(baseline_samples),
        "samples_per_cell": len(baseline_samples) // len(baseline_cells),
        "replay_zero_sql_samples": len(replay_samples),
        "overhead_min": min(overheads),
        "overhead_max": max(overheads),
        "cell_medians": ratios,
        "exposure": exposure,
    }
    subphase = _subphase_stats(subphase_samples) if subphase_samples else None
    return {"subphase": subphase, "artifact": artifact_stats, "baseline": baseline_stats}
