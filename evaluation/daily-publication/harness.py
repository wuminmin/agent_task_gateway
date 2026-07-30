#!/usr/bin/env python3
"""Reproducible RQ5 daily-publication evidence harness.

The shell driver executes production v4-offline commands.  This module owns
input rendering, digest approval, strict evidence validation, and aggregation.
It never substitutes a duration or a correctness outcome for missing evidence.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import pathlib
import statistics
import subprocess
import sys
from typing import Any


CONFIG_SCHEMA = "taskgate-daily-publication-config-v1"
PHASE_SCHEMA = "taskgate-daily-publication-phase-v1"
RESULT_SCHEMA = "taskgate-daily-publication-results-v1"
DATASET_SCHEMA = "taskgate-daily-publication-dataset-v1"
ONLINE_SCHEMA = "taskgate-daily-publication-online-evidence-v1"
ONLINE_ROUTING_MODEL = "approval_time_version_routed_retained_instances"
ONLINE_MEASUREMENT_BOUNDARY = (
    "experiment_only_router_over_four_retained_catalog_bound_gateway_services; "
    "excludes offline build_verify_activate and production routing"
)
DAYS = ("day0", "day1", "day2", "day3")
PHASES = ("build", "strict_verify", "activation")
EXPECTED_MODES = {"build": "build", "strict_verify": "verify", "activation": "activate"}
CONDITIONS = (
    "old_task_returns_old_data",
    "new_task_sees_new_data",
    "old_task_ledger_unchanged_by_switch",
    "new_publication_misses_old_cache",
    "delegated_child_uses_root_publication",
)
ONLINE_PUBLICATION_DIGEST_FIELDS = (
    "approved_input_sha256",
    "catalog_sha256",
    "bundle_manifest_sha256",
    "publication_manifest_digest",
    "dictionary_digest",
    "sidecar_digest",
    "schema_digest",
    "hot_artifact_sha256",
    "cold_artifact_sha256",
    "sidecar_artifact_sha256",
    "direct_result_sha256",
)


class EvidenceError(ValueError):
    pass


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise EvidenceError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def load_json(path: pathlib.Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as source:
            value = json.load(source, object_pairs_hook=_reject_duplicate_pairs)
    except (OSError, json.JSONDecodeError) as exc:
        raise EvidenceError(f"read {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise EvidenceError(f"{path} must contain one JSON object")
    return value


def write_json_exclusive(path: pathlib.Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        with path.open("x", encoding="utf-8") as target:
            json.dump(value, target, ensure_ascii=False, indent=2, sort_keys=True)
            target.write("\n")
    except FileExistsError as exc:
        raise EvidenceError(f"refusing to overwrite {path}") from exc


def canonical_bytes(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")


def file_sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as source:
            for block in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(block)
    except OSError as exc:
        raise EvidenceError(f"hash {path}: {exc}") from exc
    return digest.hexdigest()


def is_digest(value: Any) -> bool:
    return isinstance(value, str) and len(value) == 64 and all(character in "0123456789abcdef" for character in value)


def require_digest(value: Any, name: str) -> str:
    if not is_digest(value):
        raise EvidenceError(f"{name} must be a lowercase SHA-256")
    return value


def require_positive_number(value: Any, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
        raise EvidenceError(f"{name} must be positive and finite")
    return float(value)


def validate_config(config: dict[str, Any]) -> None:
    if config.get("schema_version") != CONFIG_SCHEMA:
        raise EvidenceError(f"config schema_version must be {CONFIG_SCHEMA}")
    if config.get("runs_per_publication") != 3:
        raise EvidenceError("runs_per_publication must be exactly 3")
    if config.get("daily_cycle_gate_ms") != 300000:
        raise EvidenceError("daily_cycle_gate_ms must be the declared five-minute gate")
    dataset = config.get("dataset")
    if not isinstance(dataset, dict):
        raise EvidenceError("dataset config is required")
    for name in ("default_rows", "publication_scale_rows", "required_row_multiple", "maximum_rows"):
        if not isinstance(dataset.get(name), int) or dataset[name] <= 0:
            raise EvidenceError(f"dataset.{name} must be a positive integer")
    if dataset["publication_scale_rows"] != 345000 or dataset["maximum_rows"] != 345000:
        raise EvidenceError("the opt-in publication-scale point must be exactly 345000 rows")
    expected_schedule = (
        ("day0", 0, 0, 0, 0),
        ("day1", 1, 1, 0, 0),
        ("day2", 2, 5, 0, 0),
        ("day3", 3, 10, 1, 1),
    )
    days = config.get("days")
    if not isinstance(days, list) or len(days) != len(expected_schedule):
        raise EvidenceError("config must define exactly day0 through day3")
    for value, expected in zip(days, expected_schedule, strict=True):
        if not isinstance(value, dict):
            raise EvidenceError("each day config must be an object")
        actual = (
            value.get("id"),
            value.get("ordinal"),
            value.get("updated_percent_from_previous"),
            value.get("inserted_percent_from_previous"),
            value.get("deleted_percent_from_previous"),
        )
        if actual != expected:
            raise EvidenceError(f"daily change schedule mismatch: got {actual}, want {expected}")
    if config.get("offline_phases") != list(PHASES):
        raise EvidenceError("offline_phases must be build, strict_verify, activation")
    if config.get("correctness_conditions") != list(CONDITIONS):
        raise EvidenceError("correctness condition membership or order changed")
    if "approval time" not in str(config.get("routing_model", "")):
        raise EvidenceError("routing_model must state approval-time publication resolution")


def validate_rows(config: dict[str, Any], rows: int) -> None:
    dataset = config["dataset"]
    multiple = dataset["required_row_multiple"]
    if rows < multiple or rows > dataset["maximum_rows"] or rows % multiple != 0:
        raise EvidenceError(f"rows must be a multiple of {multiple} between {multiple} and {dataset['maximum_rows']}")


def snapshot_input(day: str, rows: int) -> dict[str, Any]:
    physical_fields = [
        {"name": "dataset_partition", "sql_type": "smallint"},
        {"name": "l_orderkey", "sql_type": "bigint"},
        {"name": "l_linenumber", "sql_type": "integer"},
        {"name": "l_extendedprice", "sql_type": "numeric"},
    ]
    schema_digest = hashlib.sha256(canonical_bytes({"fields": physical_fields})).hexdigest()
    fields: list[dict[str, str]] = []
    for field in physical_fields:
        fields.append({**field, "canonical_field_id": field["name"]})
        fields.append({**field, "canonical_field_id": f"daily_lineitem.{field['name']}"})
    return {
        "version": "taskgate-snapshot-index-input-v1",
        "publication_name": f"daily-lineitem-{day}-r{rows}",
        "catalog_source": "daily_reporting",
        "source_relation": f"reporting.daily_lineitem_{day}",
        "ordinal_sidecar": f"taskgate_ordinal.daily_lineitem_{day}_r{rows}",
        "entity_key_fields": ["l_orderkey", "l_linenumber"],
        "snapshot": {
            "source_id": "taskgate-eval-daily-publication",
            "source_namespace": "evaluation.daily_lineitem",
            "snapshot": f"rq5-daily-lineitem-{day}-rows-{rows}",
            "schema_digest": schema_digest,
            "fields": fields,
            "rows": [],
        },
    }


def render_inputs(config_path: pathlib.Path, rows: int, output_dir: pathlib.Path) -> None:
    config = load_json(config_path)
    validate_config(config)
    validate_rows(config, rows)
    output_dir.mkdir(parents=True, exist_ok=False)
    manifest: dict[str, Any] = {
        "schema_version": "taskgate-daily-publication-input-set-v1",
        "rows": rows,
        "inputs": {},
    }
    for day in DAYS:
        value = snapshot_input(day, rows)
        path = output_dir / f"{day}.json"
        write_json_exclusive(path, value)
        manifest["inputs"][day] = {"path": path.name, "sha256": file_sha256(path)}
    write_json_exclusive(output_dir / "manifest.json", manifest)


def command_report(phase: dict[str, Any], expected_phase: str | None = None) -> dict[str, Any]:
    if phase.get("schema_version") != PHASE_SCHEMA or phase.get("status") != "pass" or phase.get("exit_code") != 0:
        raise EvidenceError("phase report does not describe a successful command")
    if expected_phase is not None and phase.get("phase") != expected_phase:
        raise EvidenceError(f"phase report is {phase.get('phase')!r}, expected {expected_phase!r}")
    value = phase.get("command_report")
    if not isinstance(value, dict):
        raise EvidenceError("phase command_report is required")
    expected_mode = EXPECTED_MODES.get(str(phase.get("phase")))
    if expected_mode is not None and value.get("mode") != expected_mode:
        raise EvidenceError(f"command mode is {value.get('mode')!r}, expected {expected_mode!r}")
    return value


def approve_input(input_path: pathlib.Path, build_report_path: pathlib.Path, output_path: pathlib.Path) -> None:
    candidate = load_json(input_path)
    phase = load_json(build_report_path)
    report = command_report(phase, "build")
    publications = report.get("publications")
    if not isinstance(publications, list) or len(publications) != 1 or not isinstance(publications[0], dict):
        raise EvidenceError("calibration build must report exactly one publication")
    publication = publications[0]
    if publication.get("publication_name") != candidate.get("publication_name"):
        raise EvidenceError("calibration publication does not match candidate input")
    candidate["expected_digests"] = {
        "sidecar_digest": require_digest(publication.get("sidecar_digest"), "sidecar_digest"),
        "dictionary_digest": require_digest(publication.get("dictionary_digest"), "dictionary_digest"),
        "manifest_digest": require_digest(publication.get("manifest_digest"), "manifest_digest"),
        "cold_payload_digest": require_digest(publication.get("cold_payload_digest"), "cold_payload_digest"),
        "hot_index_digest": require_digest(publication.get("hot_index_digest"), "hot_index_digest"),
    }
    write_json_exclusive(output_path, candidate)


def receipt_sha(report_path: pathlib.Path) -> str:
    phase = load_json(report_path)
    report = command_report(phase, "strict_verify")
    return require_digest(report.get("verification_receipt_sha256"), "verification_receipt_sha256")


def type7(values: list[float], probability: float) -> float:
    if not values:
        raise EvidenceError("cannot summarize an empty sample")
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * probability
    lower = math.floor(position)
    fraction = position - lower
    if lower + 1 >= len(ordered):
        return ordered[-1]
    return ordered[lower] + fraction * (ordered[lower + 1] - ordered[lower])


def numeric_summary(values: list[float]) -> dict[str, Any]:
    return {
        "count": len(values),
        "min": min(values),
        "p50": type7(values, 0.50),
        "p95": type7(values, 0.95),
        "max": max(values),
        "mean": statistics.fmean(values),
    }


def publication_identity(publication: dict[str, Any]) -> tuple[Any, ...]:
    keys = (
        "publication_name", "row_count", "manifest_digest", "dictionary_digest",
        "sidecar_digest", "cold_payload_digest", "hot_index_digest", "artifact_bytes", "hot_artifact_bytes",
    )
    for key in keys:
        if key.endswith("digest"):
            require_digest(publication.get(key), key)
    return tuple(publication.get(key) for key in keys)


def validate_phase_report(path: pathlib.Path, day: str, sample: int, phase_name: str) -> dict[str, Any]:
    phase = load_json(path)
    if phase.get("day") != day or phase.get("sample") != sample or phase.get("phase") != phase_name:
        raise EvidenceError(f"{path} experiment coordinates do not match its path")
    require_positive_number(phase.get("wall_ms"), f"{path}.wall_ms")
    require_positive_number(phase.get("peak_rss_bytes"), f"{path}.peak_rss_bytes")
    if phase.get("peak_rss_scope") != "root_process_vm_hwm_linux_procfs":
        raise EvidenceError(f"{path} has an unsupported RSS scope")
    report = command_report(phase, phase_name)
    publications = report.get("publications")
    if not isinstance(publications, list) or len(publications) != 1 or not isinstance(publications[0], dict):
        raise EvidenceError(f"{path} must report exactly one publication")
    publication_identity(publications[0])
    return phase


def validate_dataset_manifest(manifest: dict[str, Any], rows: int) -> None:
    if manifest.get("schema_version") != DATASET_SCHEMA:
        raise EvidenceError(f"dataset manifest schema must be {DATASET_SCHEMA}")
    row_counts = manifest.get("rows")
    if not isinstance(row_counts, dict) or any(row_counts.get(day) != rows for day in DAYS):
        raise EvidenceError("each daily publication must have the configured row count")
    expected = {
        "day1": {"updated_rows": rows // 100, "inserted_rows": 0, "deleted_rows": 0},
        "day2": {"updated_rows": rows * 5 // 100, "inserted_rows": 0, "deleted_rows": 0},
        "day3": {"updated_rows": rows * 10 // 100, "inserted_rows": rows // 100, "deleted_rows": rows // 100},
    }
    if manifest.get("changes_from_previous") != expected:
        raise EvidenceError(f"dataset changes differ from the declared schedule: {manifest.get('changes_from_previous')!r}")
    fingerprints = manifest.get("ordered_row_fingerprint_md5")
    if not isinstance(fingerprints, dict) or set(fingerprints) != set(DAYS):
        raise EvidenceError("dataset manifest must contain all ordered-row fingerprints")
    if any(not isinstance(value, str) or len(value) != 32 for value in fingerprints.values()):
        raise EvidenceError("dataset fingerprints must be PostgreSQL MD5 values")
    if len(set(fingerprints.values())) != len(DAYS):
        raise EvidenceError("all four daily publications must have distinct row fingerprints")


def gate(identifier: str, requirement: str, status: str, evidence: Any = None, reason: str = "") -> dict[str, Any]:
    value: dict[str, Any] = {"id": identifier, "requirement": requirement, "status": status}
    if evidence is not None:
        value["evidence"] = evidence
    if reason:
        value["reason"] = reason
    return value


def offline_evidence(raw_dir: pathlib.Path, runs: int, cycle_gate_ms: int) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    day_results: list[dict[str, Any]] = []
    all_samples_present = True
    invalid_reasons: list[str] = []
    all_cycle_ms: list[float] = []
    all_receipts_bound = True
    all_deterministic = True
    for day in DAYS:
        samples: list[dict[str, Any]] = []
        identities: list[tuple[Any, ...]] = []
        for sample in range(1, runs + 1):
            phase_values: dict[str, dict[str, Any]] = {}
            for phase_name in PHASES:
                path = raw_dir / day / f"sample-{sample}" / f"{phase_name}.json"
                if not path.is_file():
                    all_samples_present = False
                    continue
                try:
                    phase_values[phase_name] = validate_phase_report(path, day, sample, phase_name)
                except EvidenceError as exc:
                    invalid_reasons.append(str(exc))
            if len(phase_values) != len(PHASES):
                continue
            reports = {name: command_report(value, name) for name, value in phase_values.items()}
            publications = {name: reports[name]["publications"][0] for name in PHASES}
            build_identity = publication_identity(publications["build"])
            if publication_identity(publications["strict_verify"]) != build_identity or publication_identity(publications["activation"]) != build_identity:
                invalid_reasons.append(f"{day} sample {sample} publication identity differs across phases")
                continue
            verification_receipt = reports["strict_verify"].get("verification_receipt_sha256")
            activation_receipt = reports["activation"].get("verification_receipt_sha256")
            if not is_digest(verification_receipt) or activation_receipt != verification_receipt:
                all_receipts_bound = False
            walls = {name: float(phase_values[name]["wall_ms"]) for name in PHASES}
            peaks = {name: int(phase_values[name]["peak_rss_bytes"]) for name in PHASES}
            cycle_ms = sum(walls.values())
            all_cycle_ms.append(cycle_ms)
            identities.append(build_identity)
            samples.append({
                "sample": sample,
                "wall_ms": walls,
                "cycle_ms": cycle_ms,
                "peak_rss_bytes": peaks,
                "row_count": publications["build"]["row_count"],
                "artifact_bytes": publications["build"]["artifact_bytes"],
                "hot_artifact_bytes": publications["build"]["hot_artifact_bytes"],
                "manifest_digest": publications["build"]["manifest_digest"],
                "dictionary_digest": publications["build"]["dictionary_digest"],
                "verification_receipt_sha256": verification_receipt,
            })
        deterministic = len(identities) == runs and len(set(identities)) == 1
        all_deterministic = all_deterministic and deterministic
        day_value: dict[str, Any] = {"id": day, "samples": samples, "artifact_deterministic": deterministic}
        if samples:
            day_value["summary"] = {
                "build_ms": numeric_summary([sample["wall_ms"]["build"] for sample in samples]),
                "strict_verify_ms": numeric_summary([sample["wall_ms"]["strict_verify"] for sample in samples]),
                "activation_ms": numeric_summary([sample["wall_ms"]["activation"] for sample in samples]),
                "cycle_ms": numeric_summary([sample["cycle_ms"] for sample in samples]),
                "builder_peak_rss_bytes": numeric_summary([float(sample["peak_rss_bytes"]["build"]) for sample in samples]),
                "artifact_bytes": samples[0]["artifact_bytes"] if deterministic else None,
                "hot_artifact_bytes": samples[0]["hot_artifact_bytes"] if deterministic else None,
            }
        day_results.append(day_value)

    gates: list[dict[str, Any]] = []
    if invalid_reasons:
        gates.append(gate("offline_execution_integrity", "all phase evidence is valid", "fail", reason="; ".join(invalid_reasons[:5])))
    elif not all_samples_present:
        gates.append(gate("offline_execution_integrity", "all phase evidence is valid", "unmeasured", reason="one or more phase reports are absent"))
    else:
        gates.append(gate("offline_execution_integrity", "all phase evidence is valid", "pass"))
    gates.append(gate(
        "three_builds_per_publication", "each of four publications has at least three independent builds",
        "pass" if all_samples_present and not invalid_reasons else ("fail" if invalid_reasons else "unmeasured"),
        evidence={"required": runs, "complete_samples": sum(len(day["samples"]) for day in day_results)},
    ))
    gates.append(gate(
        "artifact_determinism", "all builds for one day have identical semantic digests and artifact sizes",
        "pass" if all_deterministic and all_samples_present and not invalid_reasons else ("fail" if invalid_reasons or (all_samples_present and not all_deterministic) else "unmeasured"),
    ))
    gates.append(gate(
        "receipt_bound_activation", "every activation uses the receipt produced by strict verification of the same artifacts",
        "pass" if all_receipts_bound and all_samples_present and not invalid_reasons else ("fail" if invalid_reasons or (all_samples_present and not all_receipts_bound) else "unmeasured"),
    ))
    if len(all_cycle_ms) == len(DAYS) * runs and not invalid_reasons:
        status = "pass" if max(all_cycle_ms) <= cycle_gate_ms else "fail"
        gates.append(gate("daily_cycle_under_five_minutes", "build + strict verification + activation <= 300000 ms for every sample", status,
                          evidence={"maximum_cycle_ms": max(all_cycle_ms), "threshold_ms": cycle_gate_ms}))
    else:
        gates.append(gate("daily_cycle_under_five_minutes", "build + strict verification + activation <= 300000 ms for every sample", "unmeasured",
                          reason="complete valid phase timings are unavailable"))
    status = "complete" if all(value["status"] == "pass" for value in gates) else ("failed" if any(value["status"] == "fail" for value in gates) else "incomplete")
    return {"status": status, "days": day_results}, gates


def validate_online_fixture(evidence: dict[str, Any]) -> dict[str, Any]:
    rows = evidence.get("rows_per_publication")
    if isinstance(rows, bool) or not isinstance(rows, int) or rows < 500 or rows > 345000 or rows % 500 != 0:
        raise EvidenceError("online rows_per_publication must be a multiple of 500 between 500 and 345000")
    boundary = evidence.get("measurement_boundary")
    if boundary != ONLINE_MEASUREMENT_BOUNDARY:
        raise EvidenceError("online measurement_boundary must identify the experiment-only retained-instance router")
    fixture = evidence.get("fixture")
    if not isinstance(fixture, dict):
        raise EvidenceError("online fixture binding is required")
    if fixture.get("fixture_class") != "correctness_fixture":
        raise EvidenceError("online fixture_class must be correctness_fixture")
    if fixture.get("rows_per_publication") != rows:
        raise EvidenceError("online fixture row count differs from the top-level row count")
    normalized_fixture: dict[str, Any] = {
        "fixture_class": fixture["fixture_class"],
        "rows_per_publication": rows,
    }
    for name in ("generator_sha256", "config_sha256", "dataset_manifest_sha256"):
        normalized_fixture[name] = require_digest(fixture.get(name), f"fixture.{name}")
    publications = fixture.get("publications")
    if not isinstance(publications, list) or len(publications) != len(DAYS):
        raise EvidenceError("online fixture must bind exactly four ordered publications")
    normalized_publications: list[dict[str, Any]] = []
    for index, publication in enumerate(publications):
        if not isinstance(publication, dict):
            raise EvidenceError("online fixture publication must be an object")
        day = DAYS[index]
        expected_name = f"daily-lineitem-{day}-r{rows}"
        row_count = publication.get("row_count")
        if publication.get("day") != day or publication.get("publication_name") != expected_name:
            raise EvidenceError(f"online fixture publication {index} identity is invalid")
        if isinstance(row_count, bool) or not isinstance(row_count, int) or row_count != rows:
            raise EvidenceError(f"online fixture publication {day} row count is invalid")
        normalized = {"day": day, "publication_name": expected_name, "row_count": row_count}
        for name in ONLINE_PUBLICATION_DIGEST_FIELDS:
            normalized[name] = require_digest(publication.get(name), f"fixture.{day}.{name}")
        normalized_publications.append(normalized)
    for name in ("catalog_sha256", "publication_manifest_digest", "direct_result_sha256"):
        if len({publication[name] for publication in normalized_publications}) != len(DAYS):
            raise EvidenceError(f"online fixture {name} must distinguish all four daily publications")
    normalized_fixture["publications"] = normalized_publications
    return {
        "rows_per_publication": rows,
        "measurement_boundary": boundary,
        "fixture": normalized_fixture,
    }


def validate_online_transition(value: dict[str, Any], source: str, target: str,
                               source_fixture: dict[str, Any] | None = None,
                               target_fixture: dict[str, Any] | None = None) -> tuple[dict[str, bool], dict[str, float]]:
    if value.get("from") != source or value.get("to") != target:
        raise EvidenceError(f"online transition must be {source}->{target}")
    timings = {
        "switch_ms": require_positive_number(value.get("switch_wall_ms"), "switch_wall_ms"),
        "first_query_ms": require_positive_number(value.get("first_query_wall_ms"), "first_query_wall_ms"),
        "replay_ms": require_positive_number(value.get("replay_wall_ms"), "replay_wall_ms"),
    }
    old = value.get("old_task")
    new = value.get("new_task")
    ledger = value.get("old_ledger")
    cache = value.get("cache")
    delegation = value.get("delegation")
    if not all(isinstance(item, dict) for item in (old, new, ledger, cache, delegation)):
        raise EvidenceError("online transition misses a correctness evidence object")
    digest_fields = (
        (old, "publication_digest_before"), (old, "publication_digest_after"),
        (old, "expected_publication_digest"),
        (old, "result_sha256_before"), (old, "result_sha256_after"), (old, "expected_result_sha256"),
        (new, "publication_digest"), (new, "expected_publication_digest"),
        (new, "result_sha256"), (new, "expected_result_sha256"),
        (ledger, "before_switch_sha256"), (ledger, "after_switch_sha256"),
        (cache, "old_cache_key_sha256"), (cache, "first_new_cache_key_sha256"),
        (cache, "replay_new_cache_key_sha256"),
        (delegation, "root_publication_digest"), (delegation, "child_publication_digest"),
    )
    for owner, name in digest_fields:
        require_digest(owner.get(name), name)
    checks = {
        "old_task_returns_old_data": (
            old["publication_digest_before"] == old["publication_digest_after"]
            and old["publication_digest_before"] == old["expected_publication_digest"]
            and old["result_sha256_before"] == old["result_sha256_after"]
            and old["result_sha256_before"] == old["expected_result_sha256"]
            and (source_fixture is None or (
                old["expected_publication_digest"] == source_fixture["publication_manifest_digest"]
                and old["expected_result_sha256"] == source_fixture["direct_result_sha256"]
            ))
        ),
        "new_task_sees_new_data": (
            new["publication_digest"] == new["expected_publication_digest"]
            and new["result_sha256"] == new["expected_result_sha256"]
            and new["result_sha256"] != old["result_sha256_after"]
            and (target_fixture is None or (
                new["expected_publication_digest"] == target_fixture["publication_manifest_digest"]
                and new["expected_result_sha256"] == target_fixture["direct_result_sha256"]
            ))
        ),
        "old_task_ledger_unchanged_by_switch": ledger["before_switch_sha256"] == ledger["after_switch_sha256"],
        "new_publication_misses_old_cache": (
            cache["old_cache_key_sha256"] != cache["first_new_cache_key_sha256"]
            and cache.get("first_new_semantic_replay") is False
            and cache["first_new_cache_key_sha256"] == cache["replay_new_cache_key_sha256"]
            and cache.get("replay_new_semantic_replay") is True
        ),
        "delegated_child_uses_root_publication": (
            isinstance(delegation.get("root_task_id"), str) and delegation.get("root_task_id")
            and delegation.get("child_root_task_id") == delegation.get("root_task_id")
            and isinstance(delegation.get("child_parent_task_id"), str) and delegation.get("child_parent_task_id")
            and delegation["root_publication_digest"] == delegation["child_publication_digest"]
            and delegation["root_publication_digest"] == new["expected_publication_digest"]
            and (target_fixture is None or
                 delegation["root_publication_digest"] == target_fixture["publication_manifest_digest"])
        ),
    }
    return checks, timings


def online_evidence(path: pathlib.Path | None) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    if path is None:
        gates = [gate(condition, condition.replace("_", " "), "unmeasured", reason="online version-routed evidence was not supplied") for condition in CONDITIONS]
        gates.insert(0, gate("online_transition_measurements", "switch, first-query, and replay latency are measured for day0->day1->day2->day3", "unmeasured",
                             reason="online version-routed evidence was not supplied"))
        return {
            "status": "not_measured", "rows_per_publication": None,
            "routing_model": None, "measurement_boundary": None, "fixture": None,
            "transitions": [], "latency_ms": None, "evidence_sha256": None,
        }, gates
    evidence = load_json(path)
    if evidence.get("schema_version") != ONLINE_SCHEMA:
        raise EvidenceError(f"online evidence schema must be {ONLINE_SCHEMA}")
    if evidence.get("routing_model") != ONLINE_ROUTING_MODEL:
        raise EvidenceError("online evidence must use approval-time binding with retained version instances")
    fixture_binding = validate_online_fixture(evidence)
    transitions = evidence.get("transitions")
    if not isinstance(transitions, list) or len(transitions) != 3:
        raise EvidenceError("online evidence must contain exactly three transitions")
    combined = {condition: True for condition in CONDITIONS}
    timing_values = {"switch_ms": [], "first_query_ms": [], "replay_ms": []}
    derived: list[dict[str, Any]] = []
    for index, value in enumerate(transitions):
        if not isinstance(value, dict):
            raise EvidenceError("online transition must be an object")
        publications = fixture_binding["fixture"]["publications"]
        checks, timings = validate_online_transition(
            value, DAYS[index], DAYS[index + 1], publications[index], publications[index + 1],
        )
        for condition in CONDITIONS:
            combined[condition] = combined[condition] and bool(checks[condition])
        for name, measurement in timings.items():
            timing_values[name].append(measurement)
        derived.append({"from": DAYS[index], "to": DAYS[index + 1], "timings": timings, "checks": checks})
    gates = [gate("online_transition_measurements", "switch, first-query, and replay latency are measured for all three transitions", "pass")]
    gates.extend(gate(condition, condition.replace("_", " "), "pass" if combined[condition] else "fail") for condition in CONDITIONS)
    status = "complete" if all(combined.values()) else "failed"
    return {
        "status": status,
        "routing_model": ONLINE_ROUTING_MODEL,
        "rows_per_publication": fixture_binding["rows_per_publication"],
        "measurement_boundary": fixture_binding["measurement_boundary"],
        "fixture": fixture_binding["fixture"],
        "transitions": derived,
        "latency_ms": {name: numeric_summary(values) for name, values in timing_values.items()},
        "evidence_sha256": file_sha256(path),
    }, gates


def git_provenance(repository: pathlib.Path) -> dict[str, Any]:
    try:
        commit = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=repository, check=True, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
        ).stdout.strip()
        dirty = bool(subprocess.run(
            ["git", "status", "--porcelain"], cwd=repository, check=True, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
        ).stdout.strip())
        return {"commit": commit, "dirty": dirty}
    except (OSError, subprocess.CalledProcessError):
        return {"commit": None, "dirty": None}


def source_provenance(repository: pathlib.Path) -> dict[str, Any]:
    fixed = (
        ".dockerignore",
        "Makefile",
        "go.mod",
        "go.sum",
        "evaluation/daily-publication/Dockerfile",
        "evaluation/daily-publication/config.json",
        "evaluation/daily-publication/compose.yaml",
        "evaluation/daily-publication/harness.py",
        "evaluation/daily-publication/run.sh",
        "evaluation/daily-publication/sql/05-generate-daily-data.sh",
        "evaluation/daily-publication/sql/10-reader.sh",
        "evaluation/daily-publication/sql/dataset-manifest.sql",
        "evaluation/daily-publication-online/Dockerfile",
        "evaluation/daily-publication-online/README.md",
        "evaluation/daily-publication-online/compose.yaml",
        "evaluation/daily-publication-online/run.sh",
        "evaluation/daily-publication-online/validate.py",
        "evaluation/daily-publication-online/sql/10-online-runtime.sh",
        "evaluation/daily-publication-online/sql/20-clone-retained-databases.sh",
        "db/init/00-schema.sql",
    )
    paths = {pathlib.Path(value) for value in fixed}
    for directory in (
        "evaluation/daily-publication/cmd/phase",
        "evaluation/cmd/rq5-online-transition",
        "evaluation/cmd/v4-offline",
        "cmd/snapshot-sidecar-install",
        "internal/approval",
        "internal/catalog",
        "internal/control",
        "internal/dataconnector",
        "internal/domain",
        "internal/gateway",
        "internal/mcp",
        "internal/snapshotbundle",
        "internal/ordinal",
        "internal/exposure",
    ):
        paths.update(
            path.relative_to(repository)
            for path in (repository / directory).rglob("*")
            if path.is_file() and path.suffix in {".go", ".sql"}
            and not path.name.endswith("_test.go")
        )
    files: dict[str, str] = {}
    for relative in sorted(paths, key=lambda value: str(value)):
        path = repository / relative
        if not path.is_file():
            raise EvidenceError(f"source provenance file is absent: {relative}")
        files[str(relative)] = file_sha256(path)
    return {
        "algorithm": "sha256",
        "combined_sha256": hashlib.sha256(canonical_bytes(files)).hexdigest(),
        "files": files,
    }


def display_path(path: pathlib.Path, repository: pathlib.Path) -> str:
    try:
        return str(path.resolve().relative_to(repository.resolve()))
    except ValueError:
        return str(path)


def raw_evidence_provenance(run_directory: pathlib.Path) -> dict[str, Any]:
    files: dict[str, str] = {}
    if run_directory.is_dir():
        for path in sorted(run_directory.rglob("*.json")):
            if path.name == "results.json" or not path.is_file():
                continue
            relative = path.relative_to(run_directory)
            files[str(relative)] = file_sha256(path)
    return {
        "algorithm": "sha256",
        "combined_sha256": hashlib.sha256(canonical_bytes(files)).hexdigest(),
        "files": files,
    }


def classify_fixture_relation(offline_rows: int, offline_dataset_sha256: str,
                              online: dict[str, Any]) -> str:
    if online.get("status") == "not_measured":
        return "not_measured"
    online_rows = online.get("rows_per_publication")
    fixture = online.get("fixture")
    if not isinstance(online_rows, int) or not isinstance(fixture, dict):
        return "unavailable"
    if online_rows != offline_rows:
        return "separate_correctness_and_scale_fixtures"
    if fixture.get("dataset_manifest_sha256") == offline_dataset_sha256:
        return "same_dataset_distinct_attested_artifacts"
    return "same_scale_distinct_fixture"


def summarize(config_path: pathlib.Path, rows: int, raw_dir: pathlib.Path,
              dataset_manifest_path: pathlib.Path, online_path: pathlib.Path | None) -> dict[str, Any]:
    repository = pathlib.Path(__file__).resolve().parents[2]
    config = load_json(config_path)
    validate_config(config)
    validate_rows(config, rows)
    dataset = load_json(dataset_manifest_path)
    dataset_error = ""
    try:
        validate_dataset_manifest(dataset, rows)
        dataset_status = "pass"
    except EvidenceError as exc:
        dataset_status = "fail"
        dataset_error = str(exc)
    offline, offline_gates = offline_evidence(raw_dir, config["runs_per_publication"], config["daily_cycle_gate_ms"])
    try:
        online, online_gates = online_evidence(online_path)
        if online["status"] != "not_measured":
            fixture = online["fixture"]
            if fixture["config_sha256"] != file_sha256(config_path):
                raise EvidenceError("online fixture config digest differs from the summarized config")
            generator_path = repository / "evaluation/daily-publication/sql/05-generate-daily-data.sh"
            if fixture["generator_sha256"] != file_sha256(generator_path):
                raise EvidenceError("online fixture generator digest differs from the summarized source")
    except EvidenceError as exc:
        online = {
            "status": "failed", "routing_model": None, "rows_per_publication": None,
            "measurement_boundary": None, "fixture": None,
            "transitions": [], "latency_ms": None, "evidence_sha256": None,
        }
        online_gates = [gate("online_transition_measurements", "valid online evidence for all transitions", "fail", reason=str(exc))]
        online_gates.extend(gate(condition, condition.replace("_", " "), "fail", reason="online evidence failed validation") for condition in CONDITIONS)
    online["fixture_relation_to_offline"] = classify_fixture_relation(
        rows, file_sha256(dataset_manifest_path), online,
    )
    gates = [gate("four_version_dataset", "Day0 plus exact 1%, 5%, and 10% update publications; Day3 also has inserts/deletes",
                  dataset_status, evidence={"rows_per_publication": rows}, reason=dataset_error)]
    gates.extend(offline_gates)
    gates.extend(online_gates)
    if any(value["status"] == "fail" for value in gates):
        acceptance = "fail"
        status = "failed"
    elif all(value["status"] == "pass" for value in gates):
        acceptance = "pass"
        status = "complete"
    else:
        acceptance = "incomplete"
        status = "incomplete"
    return {
        "schema_version": RESULT_SCHEMA,
        "status": status,
        "acceptance": acceptance,
        "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "rq": "Can TaskGate safely refresh a daily reporting publication while preserving task binding, ledger isolation, and replay separation?",
        "configuration": {
            "path": display_path(config_path, repository),
            "sha256": file_sha256(config_path),
            "rows_per_publication": rows,
            "runs_per_publication": config["runs_per_publication"],
            "daily_cycle_gate_ms": config["daily_cycle_gate_ms"],
            "routing_model": config["routing_model"],
        },
        "provenance": {
            "git": git_provenance(repository),
            "exact_source": source_provenance(repository),
            "raw_evidence": raw_evidence_provenance(raw_dir.parent),
            "dataset_manifest_path": display_path(dataset_manifest_path, repository),
            "dataset_manifest_sha256": file_sha256(dataset_manifest_path),
            "raw_directory": display_path(raw_dir, repository),
        },
        "dataset": dataset,
        "offline": offline,
        "online": online,
        "gates": gates,
        "claim_boundary": (
            "Offline build/verification/activation evidence does not establish task binding or replay separation. "
            "RQ5 passes only when the version-routed online evidence contract also passes all five correctness conditions."
        ),
    }


def pending_result(config_path: pathlib.Path) -> dict[str, Any]:
    config = load_json(config_path)
    validate_config(config)
    gates = [
        gate("four_version_dataset", "four generated daily versions", "unmeasured", reason="campaign has not been run"),
        gate("three_builds_per_publication", "three builds per publication", "unmeasured", reason="campaign has not been run"),
        gate("daily_cycle_under_five_minutes", "build + strict verification + activation <= 300000 ms", "unmeasured", reason="campaign has not been run"),
        gate("online_transition_measurements", "switch, first-query, and replay latency", "unmeasured", reason="version-routed deployment evidence has not been supplied"),
    ]
    gates.extend(gate(condition, condition.replace("_", " "), "unmeasured", reason="version-routed deployment evidence has not been supplied") for condition in CONDITIONS)
    return {
        "schema_version": RESULT_SCHEMA,
        "status": "incomplete",
        "acceptance": "incomplete",
        "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "configuration": {"path": str(config_path), "sha256": file_sha256(config_path)},
        "offline": {"status": "not_measured", "days": []},
        "online": {
            "status": "not_measured", "routing_model": None,
            "rows_per_publication": None, "measurement_boundary": None,
            "fixture": None, "fixture_relation_to_offline": "not_measured",
            "transitions": [], "latency_ms": None, "evidence_sha256": None,
        },
        "gates": gates,
        "claim_boundary": "No measurements are asserted by this pending record.",
    }


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    validate = commands.add_parser("validate-config")
    validate.add_argument("--config", type=pathlib.Path, required=True)
    render = commands.add_parser("render-inputs")
    render.add_argument("--config", type=pathlib.Path, required=True)
    render.add_argument("--rows", type=int, required=True)
    render.add_argument("--output-dir", type=pathlib.Path, required=True)
    approve = commands.add_parser("approve")
    approve.add_argument("--input", type=pathlib.Path, required=True)
    approve.add_argument("--build-report", type=pathlib.Path, required=True)
    approve.add_argument("--output", type=pathlib.Path, required=True)
    receipt = commands.add_parser("receipt-sha")
    receipt.add_argument("--report", type=pathlib.Path, required=True)
    validate_online = commands.add_parser("validate-online")
    validate_online.add_argument("--evidence", type=pathlib.Path, required=True)
    summary = commands.add_parser("summarize")
    summary.add_argument("--config", type=pathlib.Path, required=True)
    summary.add_argument("--rows", type=int, required=True)
    summary.add_argument("--raw-dir", type=pathlib.Path, required=True)
    summary.add_argument("--dataset-manifest", type=pathlib.Path, required=True)
    summary.add_argument("--online-evidence", type=pathlib.Path)
    summary.add_argument("--output", type=pathlib.Path, required=True)
    pending = commands.add_parser("pending")
    pending.add_argument("--config", type=pathlib.Path, required=True)
    pending.add_argument("--output", type=pathlib.Path, required=True)
    return result


def main(argv: list[str] | None = None) -> int:
    arguments = parser().parse_args(argv)
    try:
        if arguments.command == "validate-config":
            validate_config(load_json(arguments.config))
        elif arguments.command == "render-inputs":
            render_inputs(arguments.config, arguments.rows, arguments.output_dir)
        elif arguments.command == "approve":
            approve_input(arguments.input, arguments.build_report, arguments.output)
        elif arguments.command == "receipt-sha":
            print(receipt_sha(arguments.report))
        elif arguments.command == "validate-online":
            online, gates = online_evidence(arguments.evidence)
            if online["status"] != "complete" or any(value["status"] != "pass" for value in gates):
                return 1
        elif arguments.command == "summarize":
            value = summarize(arguments.config, arguments.rows, arguments.raw_dir,
                              arguments.dataset_manifest, arguments.online_evidence)
            write_json_exclusive(arguments.output, value)
            if value["acceptance"] == "pass":
                return 0
            if value["acceptance"] == "incomplete":
                return 2
            return 1
        elif arguments.command == "pending":
            write_json_exclusive(arguments.output, pending_result(arguments.config))
        else:
            raise AssertionError(arguments.command)
    except EvidenceError as exc:
        print(f"daily-publication: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
