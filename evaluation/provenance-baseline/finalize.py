#!/usr/bin/env python3
"""Bind runner evidence to container images and exact cgroup-v2 memory peaks."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import pathlib
import re
from typing import Any


HEX64 = re.compile(r"^[0-9a-f]{64}$")
REQUIRED_GATES = {
    "same_postgresql_version",
    "statement_timeout_enforced",
    "deterministic_sessions",
    "pinned_provsql",
    "identical_dataset",
    "result_equivalence",
    "novel_circuit_generation",
    "sample_completeness",
}


def _read_json(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    raw = path.read_bytes()
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError(f"{path} is not a JSON object")
    return value, raw


def _positive(value: str, label: str) -> int:
    try:
        parsed = int(value)
    except ValueError as error:
        raise ValueError(f"{label} is not an integer") from error
    if parsed <= 0:
        raise ValueError(f"{label} must be positive")
    return parsed


def _require_hex64(value: Any, label: str) -> str:
    if not isinstance(value, str) or not HEX64.fullmatch(value):
        raise ValueError(f"{label} is not a SHA-256 digest")
    return value


def _describe(values: list[float]) -> dict[str, float | int]:
    ordered = sorted(values)

    def quantile(probability: float) -> float:
        position = probability * (len(ordered) - 1)
        lower = math.floor(position)
        upper = math.ceil(position)
        if lower == upper:
            return ordered[lower]
        return ordered[lower] + (position - lower) * (ordered[upper] - ordered[lower])

    return {
        "count": len(ordered),
        "min": ordered[0],
        "p50": quantile(0.50),
        "p95": quantile(0.95),
        "max": ordered[-1],
        "mean": sum(ordered) / len(ordered),
    }


def _validate_distribution(actual: Any, values: list[float], label: str) -> None:
    if not isinstance(actual, dict) or not values:
        raise ValueError(f"{label} distribution is missing")
    expected = _describe(values)
    if actual.get("count") != expected["count"]:
        raise ValueError(f"{label} distribution count differs from samples")
    for field in ("min", "p50", "p95", "max", "mean"):
        value = actual.get(field)
        if (
            isinstance(value, bool)
            or not isinstance(value, (int, float))
            or not math.isfinite(value)
            or not math.isclose(value, expected[field], rel_tol=1e-12, abs_tol=1e-9)
        ):
            raise ValueError(f"{label} distribution differs from samples")


def _validate_report(report: dict[str, Any], config_raw: bytes) -> None:
    if report.get("schema_version") != 1 or report.get("status") != "complete_measured_campaign":
        raise ValueError("runner report is not a complete schema-version-1 campaign")
    boundary = report.get("comparison_boundary")
    if not isinstance(boundary, dict) or boundary.get("id") != "query-result-plus-provenance-representation-generation-v2":
        raise ValueError("runner report has the wrong comparison boundary")

    config = json.loads(config_raw)
    if not isinstance(config, dict) or config.get("schema_version") != 1:
        raise ValueError("preserved config is not a schema-version-1 object")
    campaign = report.get("campaign")
    expected_campaign = {
        "id": config.get("campaign_id"),
        "data_cache_strategy": config.get("data_cache_strategy"),
        "circuit_strategy": config.get("circuit_strategy"),
        "warmups_per_workload_and_system": config.get("warmups"),
        "measured_runs_per_workload_and_system": config.get("runs"),
        "order_seed": config.get("order_seed"),
        "statement_timeout_ms": config.get("statement_timeout_ms"),
    }
    if not isinstance(campaign, dict) or any(campaign.get(key) != value for key, value in expected_campaign.items()):
        raise ValueError("runner campaign metadata differs from the preserved config")

    configured_workloads = config.get("workloads")
    if not isinstance(configured_workloads, list) or not configured_workloads or type(config.get("runs")) is not int or config["runs"] <= 0:
        raise ValueError("preserved config has no valid measured workload contract")
    workload_contract: dict[str, tuple[int, int, int]] = {}
    for workload in configured_workloads:
        if not isinstance(workload, dict):
            raise ValueError("preserved config contains an invalid workload")
        workload_id = workload.get("id")
        scale = workload.get("scale")
        expected_rows = workload.get("expected_rows")
        carriers = workload.get("provenance_carrier_columns")
        if (
            not isinstance(workload_id, str)
            or not workload_id
            or workload_id in workload_contract
            or type(scale) is not int
            or type(expected_rows) is not int
            or type(carriers) is not int
            or scale <= 0
            or expected_rows <= 0
            or carriers <= 0
        ):
            raise ValueError("preserved config contains an invalid workload contract")
        workload_contract[workload_id] = (scale, expected_rows, carriers)

    gates = report.get("gates")
    if not isinstance(gates, list):
        raise ValueError("runner report gates are missing")
    gate_ids = [gate.get("id") for gate in gates if isinstance(gate, dict)]
    if len(gate_ids) != len(gates) or len(set(gate_ids)) != len(gate_ids) or set(gate_ids) != REQUIRED_GATES:
        raise ValueError("runner report does not contain the exact required gate set")
    if any(gate.get("status") != "pass" for gate in gates):
        raise ValueError("runner report contains a failed gate")

    dataset = report.get("dataset")
    if not isinstance(dataset, dict) or dataset.get("equal") is not True or not isinstance(dataset.get("fingerprint_rows"), int) or dataset["fingerprint_rows"] <= 0:
        raise ValueError("runner report has no verified dataset fingerprint")
    direct_dataset = _require_hex64(dataset.get("direct_sha256"), "direct dataset digest")
    provsql_dataset = _require_hex64(dataset.get("provsql_sha256"), "ProvSQL dataset digest")
    if direct_dataset != provsql_dataset:
        raise ValueError("runner report dataset digests differ")

    samples = report.get("samples")
    summaries = report.get("summaries")
    expected_sample_count = len(workload_contract) * config["runs"] * 2
    if not isinstance(samples, list) or len(samples) != expected_sample_count or not isinstance(summaries, list) or not summaries:
        raise ValueError("runner report samples or summaries are missing")
    systems = set()
    pairs: dict[tuple[str, int], dict[str, dict[str, Any]]] = {}
    representation_digests: set[str] = set()
    sample_groups: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for sample in samples:
        duration = sample.get("duration_ms") if isinstance(sample, dict) else None
        if (
            not isinstance(sample, dict)
            or isinstance(duration, bool)
            or not isinstance(duration, (int, float))
            or not math.isfinite(duration)
            or duration <= 0
        ):
            raise ValueError("runner report contains an invalid sample")
        workload_id = sample.get("workload_id")
        iteration = sample.get("iteration")
        system = sample.get("system")
        if workload_id not in workload_contract or type(iteration) is not int or not 0 <= iteration < config["runs"]:
            raise ValueError("runner sample is outside the configured workload contract")
        scale, expected_rows, carriers = workload_contract[workload_id]
        if sample.get("scale") != scale or sample.get("rows") != expected_rows or system not in {"direct_postgresql", "provsql"}:
            raise ValueError("runner sample does not match its configured workload")
        pair = pairs.setdefault((workload_id, iteration), {})
        if system in pair:
            raise ValueError("runner report contains a duplicate system sample")
        pair[system] = sample
        sample_groups.setdefault((workload_id, system), []).append(sample)
        systems.add(system)
        _require_hex64(sample.get("result_sha256"), "sample result digest")
        if system == "provsql":
            aggregate_tokens = sample.get("aggregate_tokens")
            row_tokens = sample.get("row_tokens")
            representation_fields = sample.get("provenance_representation_fields")
            gates_before = sample.get("gates_before")
            gates_after = sample.get("gates_after")
            gate_delta = sample.get("gate_delta")
            bytes_before = sample.get("artifact_bytes_before")
            bytes_after = sample.get("artifact_bytes_after")
            byte_delta = sample.get("artifact_byte_delta")
            if (
                aggregate_tokens != expected_rows * carriers
                or row_tokens != expected_rows
                or representation_fields != aggregate_tokens + row_tokens
                or sample.get("root_types_verified") is not True
                or any(type(value) is not int for value in (gates_before, gates_after, gate_delta, bytes_before, bytes_after, byte_delta))
                or gate_delta <= 0
                or gates_after - gates_before != gate_delta
                or min(bytes_before, bytes_after) < 0
                or byte_delta < 0
                or bytes_after - bytes_before != byte_delta
            ):
                raise ValueError("ProvSQL sample lacks verified aggregate roots")
            representation_digest = _require_hex64(sample.get("provenance_representation_sha256"), "provenance representation digest")
            if representation_digest in representation_digests:
                raise ValueError("ProvSQL measured samples reuse a provenance representation")
            representation_digests.add(representation_digest)
        elif any(
            key in sample
            for key in (
                "aggregate_tokens",
                "row_tokens",
                "provenance_representation_fields",
                "provenance_representation_sha256",
                "root_types_verified",
                "gates_before",
                "gates_after",
                "gate_delta",
                "artifact_bytes_before",
                "artifact_bytes_after",
                "artifact_byte_delta",
            )
        ):
            raise ValueError("direct PostgreSQL sample contains inapplicable provenance metrics")
    if systems != {"direct_postgresql", "provsql"}:
        raise ValueError("runner report does not contain both measured systems")
    expected_pairs = {
        (workload_id, iteration)
        for workload_id in workload_contract
        for iteration in range(config["runs"])
    }
    if set(pairs) != expected_pairs or any(
        set(pair) != {"direct_postgresql", "provsql"}
        or pair["direct_postgresql"]["result_sha256"] != pair["provsql"]["result_sha256"]
        for pair in pairs.values()
    ):
        raise ValueError("runner report has incomplete or nonequivalent paired samples")

    expected_summary_keys = {
        (workload_id, system)
        for workload_id in workload_contract
        for system in ("direct_postgresql", "provsql")
    }
    summary_keys: set[tuple[str, str]] = set()
    for summary in summaries:
        if not isinstance(summary, dict):
            raise ValueError("runner report contains an invalid summary")
        summary_workload = summary.get("workload_id")
        summary_system = summary.get("system")
        if not isinstance(summary_workload, str) or not isinstance(summary_system, str):
            raise ValueError("runner report contains an invalid summary key")
        key = (summary_workload, summary_system)
        group = sample_groups.get(key, [])
        if (
            key not in expected_summary_keys
            or key in summary_keys
            or summary.get("scale") != workload_contract[summary_workload][0]
            or summary.get("samples") != config["runs"]
            or len(group) != config["runs"]
        ):
            raise ValueError("runner summary does not match the configured samples")
        _validate_distribution(summary.get("duration_ms"), [float(sample["duration_ms"]) for sample in group], "duration")
        if summary_system == "provsql":
            _validate_distribution(summary.get("gate_delta"), [float(sample["gate_delta"]) for sample in group], "gate delta")
            _validate_distribution(
                summary.get("artifact_byte_delta"),
                [float(sample["artifact_byte_delta"]) for sample in group],
                "artifact byte delta",
            )
        elif "gate_delta" in summary or "artifact_byte_delta" in summary:
            raise ValueError("direct PostgreSQL summary contains inapplicable provenance metrics")
        summary_keys.add(key)
    if summary_keys != expected_summary_keys:
        raise ValueError("runner report summaries are incomplete")

    reported_systems = report.get("systems")
    provsql_system = reported_systems.get("provsql") if isinstance(reported_systems, dict) else None
    if (
        not isinstance(provsql_system, dict)
        or provsql_system.get("extension_version") != config.get("expected_provsql_version")
        or provsql_system.get("source_commit") != config.get("expected_provsql_commit")
    ):
        raise ValueError("ProvSQL system evidence differs from the preserved config")

    provenance = report.get("provenance")
    if not isinstance(provenance, dict):
        raise ValueError("runner report provenance binding is missing")
    config_digest = _require_hex64(provenance.get("config_sha256"), "config digest")
    _require_hex64(provenance.get("executable_sha256"), "executable digest")
    if hashlib.sha256(config_raw).hexdigest() != config_digest:
        raise ValueError("preserved config bytes do not match the runner binding")


def finalize(
    report_path: pathlib.Path,
    config_path: pathlib.Path,
    output_path: pathlib.Path,
    direct_image: str,
    provsql_image: str,
    provsql_revision: str,
    direct_peak: str,
    provsql_peak: str,
) -> dict[str, Any]:
    report, raw = _read_json(report_path)
    config_raw = config_path.read_bytes()
    json.loads(config_raw)
    _validate_report(report, config_raw)
    configured_revision = report.get("systems", {}).get("provsql", {}).get("source_commit")
    if configured_revision != provsql_revision or not re.fullmatch(r"[0-9a-f]{40}", provsql_revision):
        raise ValueError("ProvSQL image revision differs from the configured peeled commit")
    if not HEX64.fullmatch(direct_image.removeprefix("sha256:")):
        raise ValueError("direct PostgreSQL image ID is not a SHA-256 digest")
    if not HEX64.fullmatch(provsql_image.removeprefix("sha256:")):
        raise ValueError("ProvSQL image ID is not a SHA-256 digest")

    result = dict(report)
    result["schema_version"] = 2
    result["container_evidence"] = {
        "images": {
            "direct_postgresql": direct_image,
            "provsql": provsql_image,
            "provsql_source_revision_label": provsql_revision,
        },
        "memory": {
            "scope": "container_cgroup_v2_memory_peak_over_complete_container_lifetime",
            "direct_postgresql_peak_bytes": _positive(direct_peak, "direct peak"),
            "provsql_peak_bytes": _positive(provsql_peak, "ProvSQL peak"),
            "warning": "Lifetime peaks include initialization and warmup, not only measured query intervals.",
        },
    }
    result["provenance"] = dict(result["provenance"])
    result["provenance"]["runner_report_sha256"] = hashlib.sha256(raw).hexdigest()
    result["provenance"]["preserved_config_path"] = config_path.name

    output_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with output_path.open("x", encoding="utf-8") as output:
        json.dump(result, output, indent=2, sort_keys=True)
        output.write("\n")
    return result


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", required=True, type=pathlib.Path)
    parser.add_argument("--config", required=True, type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path)
    parser.add_argument("--direct-image", required=True)
    parser.add_argument("--provsql-image", required=True)
    parser.add_argument("--provsql-revision", required=True)
    parser.add_argument("--direct-memory-peak", required=True)
    parser.add_argument("--provsql-memory-peak", required=True)
    args = parser.parse_args()
    finalize(
        args.report,
        args.config,
        args.output,
        args.direct_image,
        args.provsql_image,
        args.provsql_revision,
        args.direct_memory_peak,
        args.provsql_memory_peak,
    )


if __name__ == "__main__":
    main()
