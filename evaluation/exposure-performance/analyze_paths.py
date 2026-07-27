#!/usr/bin/env python3
"""Derive novel/hit path and storage evidence from the recorded RQ4 trials."""

from __future__ import annotations

import hashlib
import json
import statistics
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
CAMPAIGN = Path(__file__).with_name("results.json")
OUTPUT = Path(__file__).with_name("path_analysis.json")


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def percentile(values: list[float], probability: float) -> float:
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * probability
    lower = int(position)
    upper = min(lower + 1, len(ordered) - 1)
    weight = position - lower
    return ordered[lower] + (ordered[upper] - ordered[lower]) * weight


def distribution(samples: list[dict]) -> dict:
    values = [float(sample["latency_ms"]) for sample in samples]
    return {
        "samples": len(values),
        "p50_ms": percentile(values, 0.50),
        "p95_ms": percentile(values, 0.95),
        "mean_ms": statistics.mean(values),
    }


def aggregate(name: str, per_trial: list[dict]) -> dict:
    result = {
        "path": name,
        "samples": sum(item["samples"] for item in per_trial),
        "samples_per_trial": [item["samples"] for item in per_trial],
        "latency_ms": {},
    }
    for metric in ("p50_ms", "p95_ms", "mean_ms"):
        values = [item[metric] for item in per_trial]
        result["latency_ms"][metric.removesuffix("_ms")] = statistics.median(values)
        result["latency_ms"][metric.removesuffix("_ms") + "_trial_range"] = [min(values), max(values)]
    return result


def main() -> None:
    campaign = json.loads(CAMPAIGN.read_text(encoding="utf-8"))
    if campaign.get("schema_version") != 1 or campaign.get("trials") != 3:
        raise SystemExit("the recorded RQ4 campaign is incomplete")

    raw_root = Path(__file__).with_name("raw")
    trial_paths: dict[str, list[dict]] = {"fresh_deployment_novel": [], "ramp_novel": [], "ramp_hit": []}
    raw_provenance = []
    for provenance in campaign["raw_provenance"]:
        run_id = provenance["run_id"]
        samples_path = raw_root / run_id / "samples.jsonl"
        if digest(samples_path) != provenance["samples_sha256"]:
            raise SystemExit(f"sample digest mismatch in {run_id}")
        samples = [json.loads(line) for line in samples_path.read_text(encoding="utf-8").splitlines() if line.strip()]
        ramp = [sample for sample in samples if sample["phase"] == "full_history_ramp"]
        ramp.sort(key=lambda sample: (sample["worker"], sample["iteration"]))
        if len(ramp) != 32:
            raise SystemExit(f"unexpected ramp size in {run_id}")
        novel = [sample for sample in ramp if sample["charged_release_facts"] + sample["charged_influence_facts"] > 0]
        hits = [sample for sample in ramp if sample["charged_release_facts"] + sample["charged_influence_facts"] == 0]
        if len(novel) != 4 or len(hits) != 28 or novel[0] != ramp[0]:
            raise SystemExit(f"unexpected novel/hit partition in {run_id}")
        if any(sample["actual_release_facts"] + sample["actual_influence_facts"] == 0 for sample in hits):
            raise SystemExit(f"zero-fact request mislabeled as a history hit in {run_id}")
        trial_paths["fresh_deployment_novel"].append(distribution([ramp[0]]))
        trial_paths["ramp_novel"].append(distribution(novel))
        trial_paths["ramp_hit"].append(distribution(hits))
        raw_provenance.append({
            "run_id": run_id,
            "samples_sha256": provenance["samples_sha256"],
            "ramp_samples": len(ramp),
            "novel_samples": len(novel),
            "hit_samples": len(hits),
        })

    ramp_cell = next(
        cell for cell in campaign["cells"]
        if cell["phase"] == "full_history_ramp" and cell["concurrency"] == 1
    )
    growth = ramp_cell["ledger_growth"]
    fact_rows = int(growth["fact_rows"])
    allocated = int(growth["table_bytes"] + growth["indexes_bytes"])
    analysis = {
        "schema_version": 1,
        "status": "complete_posthoc_path_analysis",
        "source_campaign_sha256": digest(CAMPAIGN),
        "trials": campaign["trials"],
        "classification": "charged facts > 0 is novel; nonzero actual facts and zero charged facts is a history hit",
        "uncertainty": "median point estimate and min-max range of per-trial summaries across three fresh deployments",
        "raw_provenance": raw_provenance,
        "paths": [aggregate(name, values) for name, values in trial_paths.items()],
        "ledger_storage": {
            "fact_rows": fact_rows,
            "release_facts": int(growth["release_used"]),
            "dependency_facts": int(growth["influence_used"]),
            "canonical_payload_bytes": int(growth["fact_payload_bytes"]),
            "table_bytes": int(growth["table_bytes"]),
            "index_bytes": int(growth["indexes_bytes"]),
            "allocated_bytes": allocated,
            "canonical_payload_bytes_per_fact": growth["fact_payload_bytes"] / fact_rows,
            "allocated_bytes_per_fact": allocated / fact_rows,
            "physical_size_semantics": "after-minus-before PostgreSQL relation allocation; page-granular and not a marginal per-fact estimate",
        },
    }
    OUTPUT.write_text(json.dumps(analysis, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"wrote {OUTPUT.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
