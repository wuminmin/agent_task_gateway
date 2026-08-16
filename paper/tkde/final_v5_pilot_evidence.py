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


def validate_final_v5_pilot_evidence(root: Path) -> dict:
    """Return the statistics the manuscript cites from retained pilot runs."""
    manifest_path = root / MANIFEST_RELATIVE_PATH
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("version") != MANIFEST_VERSION:
        raise PilotEvidenceError("pilot evidence manifest version is not the expected one")
    if manifest.get("publication_eligible") is not False:
        raise PilotEvidenceError("pilot evidence manifest must declare itself publication-ineligible")

    artifact_runs, baseline_samples = [], []
    for entry in manifest["runs"]:
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
    return {"artifact": artifact_stats, "baseline": baseline_stats}
