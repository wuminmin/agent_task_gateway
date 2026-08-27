#!/usr/bin/env python3
"""Validate the frozen Final-V5 publication campaign the manuscript cites.

The publication campaign is ``campaign_class=publication`` and
``publication_eligible=true``: one submission commit, every profile deployed
three times fresh, plus the deployment-free Scale and Compiler sub-campaigns.
Every number this module returns is derived from retained sample bytes named by
``evaluation/final-v5-wsl2/publication-evidence-v1.json`` and checked against
the digests the campaign's own sealed records carry, so a paper macro can never
quietly move onto a rerun, a pilot, or a partially finished campaign.

The pilot counterpart lives in ``final_v5_pilot_evidence.py``; this module is
deliberately parallel to it so the two classes of evidence stay distinguishable
in the manuscript.
"""

from __future__ import annotations

import hashlib
import json
import statistics
from pathlib import Path

MANIFEST_RELATIVE_PATH = "evaluation/final-v5-wsl2/publication-evidence-v1.json"
MANIFEST_VERSION = "taskgate-final-v5-publication-evidence-v1"
CAMPAIGN_EVIDENCE_RECORD = "taskgate-final-v5-split-publication-campaign-v1"
SAMPLE_RECORD = "taskgate-final-v5-profile-campaign-sample-v1"
FRESH_EXECUTIONS = 3

BASELINE_CELLS = ("S1/SF1", "S1/SF10", "S2/SF1", "S2/SF10")
REPLAY_MODES = ("semantic_replay", "normalized_rewrite_replay", "idempotent_replay")


class PublicationEvidenceError(RuntimeError):
    """Raised when retained publication evidence does not support a paper macro."""


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _read_json(path: Path):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError) as error:
        raise PublicationEvidenceError(f"cannot read {path}: {error}") from error


def _median(values):
    if not values:
        raise PublicationEvidenceError("cannot take the median of an empty sample set")
    return statistics.median(values)


def _read_samples(path: Path):
    samples = []
    for line in path.read_bytes().decode("utf-8").splitlines():
        line = line.strip()
        if line:
            samples.append(json.loads(line))
    return samples


def _load_campaign(root: Path, manifest: dict):
    campaign = manifest["campaign"]
    campaign_root = root / campaign["path"]
    evidence_path = campaign_root / "campaign-evidence.json"
    if not evidence_path.is_file():
        raise PublicationEvidenceError("publication campaign has no campaign-evidence.json")
    observed = _digest(evidence_path)
    if observed != campaign["campaign_evidence_sha256"]:
        raise PublicationEvidenceError(
            f"campaign evidence digest {observed[:12]} differs from the manifest's "
            f"{campaign['campaign_evidence_sha256'][:12]}"
        )
    evidence = _read_json(evidence_path)
    expected = {
        "record": CAMPAIGN_EVIDENCE_RECORD,
        "status": "pass",
        "campaign_class": "publication",
        "publication_eligible": True,
        "formal_campaign": True,
        "campaign_id": campaign["id"],
        "submission_commit": campaign["submission_commit"],
        "profile_cells": campaign["profile_cells"],
        "scale_non_profile_cells": campaign["scale_non_profile_cells"],
        "compiler_non_profile_cells": campaign["compiler_non_profile_cells"],
        "total_cells": campaign["total_cells"],
    }
    for key, want in expected.items():
        if evidence.get(key) != want:
            raise PublicationEvidenceError(f"campaign evidence {key} = {evidence.get(key)!r}, manifest declares {want!r}")
    declared_total = campaign["profile_cells"] + campaign["scale_non_profile_cells"] + campaign["compiler_non_profile_cells"]
    if declared_total != campaign["total_cells"]:
        raise PublicationEvidenceError("manifest denominators do not add up to total_cells")
    profile_section = evidence.get("profile_campaign") or {}
    if profile_section.get("fresh_executions") != FRESH_EXECUTIONS or profile_section.get("cells") != campaign["profile_cells"]:
        raise PublicationEvidenceError("campaign evidence profile section does not describe three fresh executions of every profile cell")
    return campaign_root, evidence


def _load_deployment(campaign_root: Path, record_path: Path, campaign: dict):
    payload = record_path.read_bytes()
    record = json.loads(payload)
    if (
        record.get("schema_version") != 1
        or record.get("campaign_class") != "publication"
        or record.get("publication_eligible") is not True
        or record.get("formal_campaign") is not True
        or record.get("campaign_id") != campaign["id"]
        or record.get("submission_commit") != campaign["submission_commit"]
    ):
        raise PublicationEvidenceError(f"deployment record {record_path} is not a publication-class record of the declared campaign")
    samples = []
    raw_files = [file for file in record.get("files", []) if file.get("kind") == "raw_jsonl"]
    if not raw_files:
        raise PublicationEvidenceError(f"deployment record {record_path} names no raw sample files")
    for file in raw_files:
        path = campaign_root / file["path"]
        if not path.is_file() or path.is_symlink():
            raise PublicationEvidenceError(f"sample file {file['path']} is absent or unsafe")
        if _digest(path) != file["sha256"] or path.stat().st_size != file["bytes"]:
            raise PublicationEvidenceError(f"sample file {file['path']} changed since the campaign sealed it")
        for line in _read_samples(path):
            if line.get("record") != SAMPLE_RECORD or line.get("campaign_class") != "publication":
                raise PublicationEvidenceError(f"sample file {file['path']} carries a non-publication record")
            sample = line["sample"]
            if sample.get("warmup"):
                continue
            if sample.get("status") != "pass":
                raise PublicationEvidenceError(f"sample file {file['path']} retains a non-passing measured sample")
            if sample.get("publication_eligible") is not True:
                raise PublicationEvidenceError(f"sample file {file['path']} retains a publication-ineligible measured sample")
            if sample.get("campaign_id") != campaign["id"]:
                raise PublicationEvidenceError(f"sample file {file['path']} belongs to another campaign")
            samples.append(sample)
    return record, samples, hashlib.sha256(payload).hexdigest()


def _baseline_stats(samples):
    by_cell = {}
    exposure = {}
    for sample in samples:
        workload_scale = "/".join(sample["cell_id"].split("/")[:2])
        by_cell.setdefault(workload_scale, {}).setdefault(sample["mode"], []).append(sample["client_full_drain_ms"])
        if sample["mode"] == "novel":
            exposure[workload_scale] = {
                "release": sample["actual_release_facts"],
                "dependency": sample["actual_dependency_facts"],
                "rows": sample["row_count"],
            }
    missing = [cell for cell in BASELINE_CELLS if cell not in by_cell]
    if missing:
        raise PublicationEvidenceError(f"baseline publication evidence is missing cells {missing}")
    replay_samples = [sample for sample in samples if sample["mode"] in REPLAY_MODES]
    replay_deltas = {sample["business_sql_delta"] for sample in replay_samples}
    if replay_deltas != {0}:
        raise PublicationEvidenceError(f"baseline replay samples recorded business SQL deltas {sorted(replay_deltas)}")
    novel_deltas = {sample["business_sql_delta"] for sample in samples if sample["mode"] == "novel"}
    if not novel_deltas or min(novel_deltas) < 1:
        raise PublicationEvidenceError(f"baseline novel samples recorded business SQL deltas {sorted(novel_deltas)}")
    for sample in samples:
        if sample["mode"] not in ("semantic_replay", "normalized_rewrite_replay"):
            continue
        charged = (sample["charged_release_facts"], sample["charged_dependency_facts"], sample["charged_outcome_facts"])
        if charged != (0, 0, 0):
            raise PublicationEvidenceError(f"baseline replay {sample['cell_id']} charged Facts {charged}")
    ratios = {}
    for cell in sorted(by_cell):
        modes = by_cell[cell]
        for mode in ("direct", "novel"):
            if mode not in modes:
                raise PublicationEvidenceError(f"baseline cell {cell} has no {mode} samples")
        ratios[cell] = {"direct_ms": _median(modes["direct"]), "novel_ms": _median(modes["novel"])}
        for mode, key in (("semantic_replay", "semantic_ms"), ("idempotent_replay", "idempotent_ms")):
            if mode in modes:
                ratios[cell][key] = _median(modes[mode])
        ratios[cell]["novel_over_direct"] = ratios[cell]["novel_ms"] / ratios[cell]["direct_ms"]
    overheads = [value["novel_over_direct"] for value in ratios.values()]
    cells = {sample["cell_id"] for sample in samples}
    return {
        "cells": len(cells),
        "samples": len(samples),
        "samples_per_cell": len(samples) // len(cells),
        "replay_zero_sql_samples": len(replay_samples),
        "overhead_min": min(overheads),
        "overhead_max": max(overheads),
        "cell_medians": ratios,
        "exposure": exposure,
    }


def validate_final_v5_publication_evidence(root: Path) -> dict:
    """Return the statistics the manuscript cites from the publication campaign."""
    manifest_path = root / MANIFEST_RELATIVE_PATH
    manifest = _read_json(manifest_path)
    if manifest.get("version") != MANIFEST_VERSION:
        raise PublicationEvidenceError("publication evidence manifest version is not the expected one")
    if manifest.get("publication_eligible") is not True:
        raise PublicationEvidenceError("publication evidence manifest must declare itself publication-eligible")
    campaign = manifest["campaign"]
    campaign_root, evidence = _load_campaign(root, manifest)

    records = []
    by_experiment = {}
    record_digests = []
    for record_path in sorted((campaign_root / "deployments").glob("*/[0-9][0-9][0-9]/deployment-record.json")):
        record, samples, record_digest = _load_deployment(campaign_root, record_path, campaign)
        records.append(record)
        record_digests.append(record_digest)
        for sample in samples:
            by_experiment.setdefault(sample["experiment_id"], []).append(sample)
    expected_records = campaign["deployments"]
    if len(records) != expected_records:
        raise PublicationEvidenceError(f"campaign retains {len(records)} deployment records, manifest declares {expected_records}")
    if sorted(record_digests) != sorted(evidence["profile_campaign"]["evidence_sha256"]):
        raise PublicationEvidenceError("deployment records do not match the digests the campaign evidence sealed")
    aliases = {record["profile_alias"] for record in records}
    repetitions = {}
    for record in records:
        repetitions.setdefault(record["profile_alias"], set()).add(record["repetition"])
    if any(reps != set(range(1, FRESH_EXECUTIONS + 1)) for reps in repetitions.values()):
        raise PublicationEvidenceError("a profile was not deployed exactly three times")
    profile_cells = {cell for record in records for cell in record["cells"]}
    if len(profile_cells) != campaign["profile_cells"]:
        raise PublicationEvidenceError(f"deployment records cover {len(profile_cells)} profile cells, manifest declares {campaign['profile_cells']}")

    non_profile = {}
    for campaign_id, section in (evidence.get("non_profile_campaigns") or {}).items():
        sealed = campaign_root / "non-profile" / campaign_id / "evidence" / "manifest.json"
        if not sealed.is_file() or [_digest(sealed)] != section.get("evidence_sha256"):
            raise PublicationEvidenceError(f"non-profile campaign {campaign_id} sealed evidence is absent or changed")
        non_profile[campaign_id] = {"cells": section["cells"], "fresh_executions": section["fresh_executions"]}

    stats = {
        "campaign_id": campaign["id"],
        "commit": campaign["submission_commit"],
        "deployments": len(records),
        "profiles": len(aliases),
        "fresh_executions": FRESH_EXECUTIONS,
        "profile_cells": len(profile_cells),
        "scale_non_profile_cells": campaign["scale_non_profile_cells"],
        "compiler_non_profile_cells": campaign["compiler_non_profile_cells"],
        "total_cells": campaign["total_cells"],
        "measured_samples": sum(len(samples) for samples in by_experiment.values()),
        "samples_by_experiment": {experiment: len(samples) for experiment, samples in sorted(by_experiment.items())},
        "non_profile": non_profile,
    }
    if "baseline" in by_experiment:
        stats["baseline"] = _baseline_stats(by_experiment["baseline"])
    else:
        raise PublicationEvidenceError("publication campaign retains no baseline samples")
    return stats
