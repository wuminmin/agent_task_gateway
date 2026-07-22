#!/usr/bin/env python3
"""Generate deterministic paper artifacts exclusively from completed raw runs."""

from __future__ import annotations

import argparse
import csv
import hashlib
import html
import importlib.util
import json
import math
import pathlib
import re
import sys
from collections import defaultdict
from typing import Any, Iterable


BASELINE_ORDER = {
    "direct_postgresql": 0,
    "native_view_rls": 1,
    "ast_only_gateway": 2,
    "full_taskgate": 3,
}
BASELINE_LABEL = {
    "direct_postgresql": "Direct PostgreSQL",
    "native_view_rls": "Native View/RLS",
    "ast_only_gateway": "AST-only Gateway",
    "full_taskgate": "Full TaskGate",
}
COLORS = {
    "direct_postgresql": "#4477AA",
    "native_view_rls": "#66CCEE",
    "ast_only_gateway": "#228833",
    "full_taskgate": "#CC6677",
}
REQUIRED_FULL_SUITES = {
    "taskgate-sf1-four-baseline": {"tpch_sf1", "tpcds_sf1"},
    "taskgate-sf10-four-baseline": {"tpch_sf10", "tpcds_sf10"},
}
REQUIRED_FULL_EXPERIMENTS = {
    "tpch_sf1": ("tpch", 1),
    "tpcds_sf1": ("tpcds", 1),
    "tpch_sf10": ("tpch", 10),
    "tpcds_sf10": ("tpcds", 10),
}
REQUIRED_FULL_CONCURRENCY = {1, 8, 32}
SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
CAMPAIGN_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


def fail(message: str) -> None:
    raise SystemExit(f"artifact generation failed: {message}")


def read_json(path: pathlib.Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot read {path}: {exc}")


def read_jsonl(path: pathlib.Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    try:
        with path.open(encoding="utf-8") as source:
            for line_number, line in enumerate(source, 1):
                if not line.strip():
                    continue
                value = json.loads(line)
                if not isinstance(value, dict):
                    fail(f"{path}:{line_number} is not a JSON object")
                rows.append(value)
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot read {path}: {exc}")
    return rows


def sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def relative(path: pathlib.Path, root: pathlib.Path) -> str:
    try:
        return path.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return path.resolve().as_posix()


def full_campaign_error(
    items: list[tuple[pathlib.Path, dict[str, Any]]],
) -> tuple[str | None, pathlib.Path | None]:
    if len(items) != len(REQUIRED_FULL_SUITES):
        return f"expected exactly {len(REQUIRED_FULL_SUITES)} full suite runs, got {len(items)}", None
    by_suite: dict[str, tuple[pathlib.Path, dict[str, Any]]] = {}
    for item in items:
        suite = item[1].get("suite")
        if suite not in REQUIRED_FULL_SUITES:
            return f"unexpected full suite {suite!r}", None
        if suite in by_suite:
            return f"campaign contains multiple completed runs for {suite}", None
        by_suite[str(suite)] = item
    if set(by_suite) != set(REQUIRED_FULL_SUITES):
        missing = sorted(set(REQUIRED_FULL_SUITES) - set(by_suite))
        return f"campaign is missing required suites: {', '.join(missing)}", None

    campaigns = {str(metadata.get("campaign_id", "")) for _, metadata in items}
    if len(campaigns) != 1 or not CAMPAIGN_ID_PATTERN.fullmatch(next(iter(campaigns), "")):
        return "full suites do not share one valid nonempty campaign_id", None
    campaign_id = next(iter(campaigns))
    revisions = {str(metadata.get("git_revision", "")) for _, metadata in items}
    if len(revisions) != 1 or next(iter(revisions), "") in {"", "unknown"}:
        return "full suites do not share one known git_revision", None
    revision = next(iter(revisions))
    if any(metadata.get("git_dirty") is not False for _, metadata in items):
        return "full suites must all record git_dirty=false", None
    for field in (
        "go_version",
        "goos",
        "goarch",
        "baseline_order",
        "concurrency",
        "warmup_runs_per_worker",
        "measured_runs_per_worker",
    ):
        values = {json.dumps(metadata.get(field), sort_keys=True) for _, metadata in items}
        if len(values) != 1:
            return f"full suites have inconsistent {field}", None

    parents = {directory.parent.resolve() for directory, _ in items}
    if len(parents) != 1:
        return "full suite directories do not share one raw-data root", None
    manifest_path = next(iter(parents)) / f"campaign-{campaign_id}.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return f"cannot read linked campaign manifest {manifest_path}: {exc}", None
    if not isinstance(manifest, dict):
        return f"linked campaign manifest is not a JSON object: {manifest_path}", None
    if (
        manifest.get("schema_version") != 1
        or manifest.get("campaign_id") != campaign_id
        or manifest.get("mode") != "full"
        or manifest.get("status") != "complete"
        or manifest.get("git_revision") != revision
        or manifest.get("git_dirty") is not False
    ):
        return f"linked campaign manifest identity/status/provenance is invalid: {manifest_path}", None
    declared_runs = manifest.get("runs")
    if not isinstance(declared_runs, list) or len(declared_runs) != len(items):
        return f"linked campaign manifest must declare exactly both runs: {manifest_path}", None
    expected_runs = {
        (str(metadata.get("run_id", "")), str(metadata.get("suite", ""))): sha256(directory / "run.json")
        for directory, metadata in items
    }
    actual_runs: dict[tuple[str, str], str] = {}
    for entry in declared_runs:
        if not isinstance(entry, dict):
            return f"linked campaign manifest contains a non-object run: {manifest_path}", None
        key = (str(entry.get("run_id", "")), str(entry.get("suite", "")))
        digest = str(entry.get("run_json_sha256", ""))
        if key in actual_runs or not SHA256_PATTERN.fullmatch(digest):
            return f"linked campaign manifest contains a duplicate or invalid run digest: {manifest_path}", None
        actual_runs[key] = digest
    if actual_runs != expected_runs:
        return f"linked campaign manifest run IDs or run.json digests do not match: {manifest_path}", None
    return None, manifest_path


def discover_runs(raw_root: pathlib.Path, explicit: list[pathlib.Path], allow_empty: bool = False) -> list[pathlib.Path]:
    candidates = explicit or sorted(path.parent for path in raw_root.glob("*/run.json"))
    completed: list[tuple[pathlib.Path, dict[str, Any]]] = []
    for directory in candidates:
        metadata_path = directory / "run.json"
        if not metadata_path.is_file():
            fail(f"run directory has no run.json: {directory}")
        metadata = read_json(metadata_path)
        if not isinstance(metadata, dict):
            fail(f"run metadata is not a JSON object: {metadata_path}")
        if metadata.get("schema_version") != 1:
            fail(f"unsupported raw schema in {metadata_path}")
        if metadata.get("status") != "complete":
            if explicit:
                fail(f"explicit run is not complete: {directory}")
            continue
        completed.append((directory, metadata))
    if not completed:
        if allow_empty and not explicit:
            return []
        fail("no completed raw evaluation runs were found; run make eval-smoke or make eval-full")
    full = [item for item in completed if item[1].get("mode") == "full"]
    if explicit:
        if full:
            if len(full) != len(completed):
                fail("explicit run selection mixes full and non-full suites")
            error, _ = full_campaign_error(full)
            if error:
                fail(f"explicit full run selection is not a publishable campaign: {error}")
            return [item[0] for item in sorted(full, key=lambda item: str(item[1].get("suite", "")))]
        modes = {str(metadata.get("mode", "")) for _, metadata in completed}
        if modes != {"smoke"}:
            fail("explicit non-full runs must all be smoke runs")
        return [item[0] for item in completed]

    if full:
        grouped: dict[str, list[tuple[pathlib.Path, dict[str, Any]]]] = defaultdict(list)
        missing_campaign = 0
        for item in full:
            campaign_id = str(item[1].get("campaign_id", ""))
            if campaign_id:
                grouped[campaign_id].append(item)
            else:
                missing_campaign += 1
        valid: list[list[tuple[pathlib.Path, dict[str, Any]]]] = []
        reasons: list[str] = []
        if missing_campaign:
            reasons.append(f"{missing_campaign} completed full run(s) omit campaign_id")
        for campaign_id, items in sorted(grouped.items()):
            error, _ = full_campaign_error(items)
            if error:
                reasons.append(f"{campaign_id}: {error}")
            else:
                valid.append(items)
        if not valid:
            if allow_empty:
                return []
            detail = "; ".join(reasons) or "no linked campaign was found"
            fail(f"completed full runs exist, but none form a publishable SF1+SF10 campaign: {detail}")
        selected = max(
            valid,
            key=lambda items: max(str(metadata.get("finished_at", "")) for _, metadata in items),
        )
        return [item[0] for item in sorted(selected, key=lambda item: str(item[1].get("suite", "")))]

    pool = completed
    latest_by_suite: dict[str, tuple[pathlib.Path, dict[str, Any]]] = {}
    for item in pool:
        suite = str(item[1].get("suite", ""))
        if not suite:
            fail(f"run omits suite: {item[0]}")
        previous = latest_by_suite.get(suite)
        finished = str(item[1].get("finished_at", ""))
        if previous is None or finished > str(previous[1].get("finished_at", "")):
            latest_by_suite[suite] = item
    return [latest_by_suite[key][0] for key in sorted(latest_by_suite)]


def percentile(values: Iterable[float], probability: float) -> float | None:
    ordered = sorted(float(value) for value in values)
    if not ordered:
        return None
    if len(ordered) == 1:
        return ordered[0]
    # Hyndman-Fan type 7, the default used by R and NumPy's linear method.
    position = (len(ordered) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    fraction = position - lower
    return ordered[lower] + (ordered[upper] - ordered[lower]) * fraction


def rounded(value: float | int | None) -> float | int | None:
    if value is None or isinstance(value, int):
        return value
    return round(float(value), 6)


def sum_map(values: dict[str, Any] | None) -> float | None:
    if not values:
        return None
    return sum(float(value) for value in values.values())


def max_map(values: dict[str, Any] | None) -> int | None:
    if not values:
        return None
    return max(int(value) for value in values.values())


def repository_file(root: pathlib.Path, candidate: pathlib.Path, label: str) -> pathlib.Path:
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as exc:
        fail(f"{label} must resolve to a file inside the repository: {candidate} ({exc})")
    if not resolved.is_file():
        fail(f"{label} is not a regular file: {resolved}")
    return resolved


def verified_input_path(root: pathlib.Path, value: Any, digest: Any, label: str) -> pathlib.Path:
    if not isinstance(value, str) or not value or "\\" in value:
        fail(f"{label} path must be a nonempty repository-relative POSIX path")
    relative_path = pathlib.PurePosixPath(value)
    if relative_path.is_absolute() or ".." in relative_path.parts:
        fail(f"{label} path must not be absolute or traverse outside the repository: {value}")
    if not isinstance(digest, str) or not SHA256_PATTERN.fullmatch(digest):
        fail(f"{label} SHA-256 must be exactly 64 lowercase hexadecimal characters")
    path = repository_file(root, root / pathlib.Path(*relative_path.parts), label)
    actual = sha256(path)
    if actual != digest:
        fail(f"{label} SHA-256 mismatch for {value}: metadata={digest}, actual={actual}")
    return path


def validate_config_provenance(
    root: pathlib.Path,
    metadata: dict[str, Any],
    inputs: list[pathlib.Path],
) -> dict[str, tuple[str, int]]:
    config_path = verified_input_path(
        root,
        metadata.get("config_path"),
        metadata.get("config_sha256"),
        f"run {metadata.get('run_id')} config",
    )
    inputs.append(config_path)
    config = read_json(config_path)
    if not isinstance(config, dict):
        fail(f"evaluation config is not a JSON object: {config_path}")
    for field in ("name", "mode", "baseline_order", "concurrency", "warmup_runs_per_worker", "measured_runs_per_worker"):
        metadata_field = "suite" if field == "name" else field
        if config.get(field) != metadata.get(metadata_field):
            fail(f"run {metadata.get('run_id')} metadata does not match config field {field}")
    experiments = config.get("experiments")
    if not isinstance(experiments, list) or not experiments:
        fail(f"evaluation config has no experiments: {config_path}")
    details: dict[str, tuple[str, int]] = {}
    workload_digests = metadata.get("workload_sha256")
    if not isinstance(workload_digests, dict):
        fail(f"run {metadata.get('run_id')} omits workload_sha256 provenance")
    computed_digests: dict[str, str] = {}
    for experiment in experiments:
        if not isinstance(experiment, dict):
            fail(f"evaluation config contains a non-object experiment: {config_path}")
        experiment_id = experiment.get("id")
        family = experiment.get("family")
        scale_factor = experiment.get("scale_factor")
        if not isinstance(experiment_id, str) or not experiment_id or experiment_id in details:
            fail(f"evaluation config has an empty or duplicate experiment ID: {config_path}")
        if family not in {"tpch", "tpcds"} or type(scale_factor) is not int:
            fail(f"evaluation config has invalid family/scale for {experiment_id}")
        details[experiment_id] = (str(family), int(scale_factor))

        workload_value = experiment.get("workload")
        if not isinstance(workload_value, str) or not workload_value:
            fail(f"evaluation config omits workload for {experiment_id}")
        workload_path = repository_file(root, config_path.parent / workload_value, f"{experiment_id} workload manifest")
        inputs.append(workload_path)
        workload = read_json(workload_path)
        if (
            not isinstance(workload, dict)
            or workload.get("schema_version") != 1
            or workload.get("family") != family
            or not isinstance(workload.get("queries"), list)
            or not workload["queries"]
        ):
            fail(f"invalid workload manifest for {experiment_id}: {workload_path}")
        workload_hasher = hashlib.sha256()
        seen_queries: set[str] = set()
        for query in workload["queries"]:
            if not isinstance(query, dict) or not isinstance(query.get("id"), str) or not query["id"]:
                fail(f"invalid query entry in {workload_path}")
            query_id = query["id"]
            if query_id in seen_queries:
                fail(f"duplicate query ID {query_id} in {workload_path}")
            seen_queries.add(query_id)
            sql = query.get("sql")
            if not isinstance(sql, dict) or set(sql) != set(BASELINE_ORDER):
                fail(f"query {experiment_id}/{query_id} does not map exactly four baselines")
            workload_hasher.update(query_id.encode("utf-8"))
            for baseline in BASELINE_ORDER:
                sql_value = sql.get(baseline)
                if not isinstance(sql_value, str) or not sql_value:
                    fail(f"query {experiment_id}/{query_id}/{baseline} has no SQL path")
                sql_path = repository_file(root, workload_path.parent / sql_value, f"{experiment_id}/{query_id}/{baseline} SQL")
                inputs.append(sql_path)
                workload_hasher.update(baseline.encode("utf-8"))
                workload_hasher.update(sql_path.read_bytes())
        computed_digests[experiment_id] = workload_hasher.hexdigest()
    if set(workload_digests) != set(details):
        fail(f"run {metadata.get('run_id')} workload provenance does not cover its exact experiments")
    for experiment_id, actual in computed_digests.items():
        recorded = workload_digests.get(experiment_id)
        if not isinstance(recorded, str) or not SHA256_PATTERN.fullmatch(recorded) or recorded != actual:
            fail(f"run {metadata.get('run_id')} workload SHA-256 mismatch for {experiment_id}")
    return details


def measurement_key(row: dict[str, Any], source: pathlib.Path) -> tuple[str, str, int]:
    try:
        experiment = row["experiment"]
        baseline = row["baseline"]
        concurrency = row["concurrency"]
    except KeyError as exc:
        fail(f"observation in {source} omits {exc.args[0]}")
    if not isinstance(experiment, str) or not isinstance(baseline, str) or type(concurrency) is not int:
        fail(f"observation in {source} has invalid experiment/baseline/concurrency types")
    return experiment, baseline, concurrency


def load_measurements(root: pathlib.Path, run_dirs: list[pathlib.Path]) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[pathlib.Path], list[dict[str, Any]]]:
    samples: list[dict[str, Any]] = []
    cells: list[dict[str, Any]] = []
    inputs: list[pathlib.Path] = []
    selected: list[tuple[pathlib.Path, dict[str, Any]]] = []
    for directory in run_dirs:
        metadata = read_json(directory / "run.json")
        if not isinstance(metadata, dict) or metadata.get("schema_version") != 1 or metadata.get("status") != "complete":
            fail(f"selected run is not a completed schema-version-1 run: {directory}")
        selected.append((directory, metadata))
    full = [item for item in selected if item[1].get("mode") == "full"]
    if full:
        if len(full) != len(selected):
            fail("selected measurement set mixes full and non-full runs")
        error, manifest_path = full_campaign_error(full)
        if error or manifest_path is None:
            fail(f"selected full runs are not one publishable campaign: {error}")
        inputs.append(manifest_path)

    metadata_rows: list[dict[str, Any]] = []
    for directory, metadata in selected:
        metadata_path = directory / "run.json"
        samples_path = directory / "samples.jsonl"
        cells_path = directory / "cells.jsonl"
        for path in (metadata_path, samples_path, cells_path):
            if not path.is_file():
                fail(f"completed run is missing {path.name}: {directory}")
            inputs.append(path)
        metadata_rows.append(metadata)
        run_id = metadata.get("run_id")
        if not isinstance(run_id, str) or not run_id:
            fail(f"completed run omits run_id: {directory}")
        baseline_order = metadata.get("baseline_order")
        if baseline_order != list(BASELINE_ORDER):
            fail(f"run {run_id} must contain the exact four baseline order")
        concurrency_values = metadata.get("concurrency")
        if (
            not isinstance(concurrency_values, list)
            or not concurrency_values
            or any(type(value) is not int or value < 1 for value in concurrency_values)
            or len(set(concurrency_values)) != len(concurrency_values)
        ):
            fail(f"run {run_id} has invalid concurrency provenance")
        measured_per_worker = metadata.get("measured_runs_per_worker")
        warmup_per_worker = metadata.get("warmup_runs_per_worker")
        if type(measured_per_worker) is not int or measured_per_worker < 1 or type(warmup_per_worker) is not int or warmup_per_worker < 1:
            fail(f"run {run_id} has invalid warmup/measurement provenance")
        experiment_details = validate_config_provenance(root, metadata, inputs)
        if metadata.get("mode") == "full":
            suite = metadata.get("suite")
            if suite not in REQUIRED_FULL_SUITES or set(experiment_details) != REQUIRED_FULL_SUITES[str(suite)]:
                fail(f"full run {run_id} does not have the exact experiments required for suite {suite}")
            if any(experiment_details[key] != REQUIRED_FULL_EXPERIMENTS[key] for key in experiment_details):
                fail(f"full run {run_id} has incorrect family/scale provenance")
            if set(concurrency_values) != REQUIRED_FULL_CONCURRENCY or len(concurrency_values) != len(REQUIRED_FULL_CONCURRENCY):
                fail(f"full run {run_id} must cover exactly concurrency 1, 8, and 32")
            if measured_per_worker < 30:
                fail(f"full run has fewer than 30 measurements per worker: {directory}")

            dataset_digests = metadata.get("dataset_sha256_manifests")
            dataset_paths = metadata.get("dataset_manifest_paths")
            if not isinstance(dataset_digests, dict) or not isinstance(dataset_paths, dict):
                fail(f"full run {run_id} omits dataset provenance")
            if set(dataset_digests) != set(experiment_details) or set(dataset_paths) != set(experiment_details):
                fail(f"full run {run_id} dataset provenance does not cover its exact experiments")
            for experiment_id in sorted(experiment_details):
                inputs.append(
                    verified_input_path(
                        root,
                        dataset_paths[experiment_id],
                        dataset_digests[experiment_id],
                        f"{run_id}/{experiment_id} dataset manifest",
                    )
                )

            probe_paths = metadata.get("metrics_probe_paths")
            probe_digests = metadata.get("metrics_probe_sha256")
            if not isinstance(probe_paths, dict) or not isinstance(probe_digests, dict):
                fail(f"full run {run_id} omits metrics-probe provenance")
            if set(probe_paths) != set(experiment_details) or set(probe_digests) != set(experiment_details):
                fail(f"full run {run_id} metrics-probe provenance does not cover its exact experiments")
            for experiment_id in sorted(experiment_details):
                paths = probe_paths.get(experiment_id)
                digests = probe_digests.get(experiment_id)
                if not isinstance(paths, dict) or not isinstance(digests, dict) or set(paths) != set(BASELINE_ORDER) or set(digests) != set(BASELINE_ORDER):
                    fail(f"full run {run_id} has incomplete metrics-probe provenance for {experiment_id}")
                for baseline in BASELINE_ORDER:
                    inputs.append(
                        verified_input_path(
                            root,
                            paths[baseline],
                            digests[baseline],
                            f"{run_id}/{experiment_id}/{baseline} metrics probe",
                        )
                    )

        run_samples = read_jsonl(samples_path)
        run_cells = read_jsonl(cells_path)
        if any(row.get("schema_version") != 1 for row in run_samples + run_cells):
            fail(f"unsupported observation schema in {directory}")
        if any(row.get("success") is not True for row in run_samples):
            fail(f"completed run contains unsuccessful samples: {directory}")

        expected_cells = {
            (experiment_id, baseline, concurrency)
            for experiment_id in experiment_details
            for baseline in BASELINE_ORDER
            for concurrency in concurrency_values
        }
        cell_by_key: dict[tuple[str, str, int], dict[str, Any]] = {}
        for row in run_cells:
            key = measurement_key(row, cells_path)
            if key in cell_by_key:
                fail(f"duplicate cell in {cells_path}: {key}")
            cell_by_key[key] = row
            experiment_id, _, concurrency = key
            if row.get("run_id") != run_id or experiment_id not in experiment_details:
                fail(f"cell identity does not match run {run_id}: {key}")
            family, scale_factor = experiment_details[experiment_id]
            if row.get("family") != family or row.get("scale_factor") != scale_factor:
                fail(f"cell family/scale does not match config in run {run_id}: {key}")
            expected_samples = measured_per_worker * concurrency
            if row.get("measured_samples") != expected_samples or row.get("warmup_samples") != warmup_per_worker * concurrency:
                fail(f"cell has incomplete or surplus samples in run {run_id}: {key}")
            if metadata.get("mode") == "full" and (not row.get("cpu_seconds") or not row.get("peak_memory_bytes")):
                fail(f"full cell omits required resource metrics in run {run_id}: {key}")
        if set(cell_by_key) != expected_cells:
            missing = sorted(expected_cells - set(cell_by_key))
            extra = sorted(set(cell_by_key) - expected_cells)
            fail(f"run {run_id} does not contain its exact cell matrix; missing={missing}, extra={extra}")

        sample_pairs: dict[tuple[str, str, int], set[tuple[int, int]]] = defaultdict(set)
        for row in run_samples:
            key = measurement_key(row, samples_path)
            experiment_id, _, concurrency = key
            if key not in expected_cells or row.get("run_id") != run_id:
                fail(f"sample identity does not match run {run_id}: {key}")
            family, scale_factor = experiment_details[experiment_id]
            if row.get("family") != family or row.get("scale_factor") != scale_factor:
                fail(f"sample family/scale does not match config in run {run_id}: {key}")
            worker = row.get("worker")
            iteration = row.get("iteration")
            if type(worker) is not int or type(iteration) is not int:
                fail(f"sample has invalid worker/iteration in run {run_id}: {key}")
            pair = (worker, iteration)
            if pair in sample_pairs[key]:
                fail(f"duplicate worker/iteration sample in run {run_id}: {key}/{pair}")
            sample_pairs[key].add(pair)
        for key in expected_cells:
            concurrency = key[2]
            expected_pairs = {
                (worker, iteration)
                for worker in range(concurrency)
                for iteration in range(measured_per_worker)
            }
            if sample_pairs[key] != expected_pairs:
                fail(f"run {run_id} has incomplete or surplus sample rows for {key}")
        samples.extend(run_samples)
        cells.extend(run_cells)
    return samples, cells, inputs, metadata_rows


def build_summary(samples: list[dict[str, Any]], cells: list[dict[str, Any]]) -> list[dict[str, Any]]:
    grouped_samples: dict[tuple[Any, ...], list[dict[str, Any]]] = defaultdict(list)
    cell_by_key: dict[tuple[Any, ...], dict[str, Any]] = {}
    for row in samples:
        key = (row["experiment"], row["family"], int(row["scale_factor"]), row["baseline"], int(row["concurrency"]))
        grouped_samples[key].append(row)
    for row in cells:
        key = (row["experiment"], row["family"], int(row["scale_factor"]), row["baseline"], int(row["concurrency"]))
        if key in cell_by_key:
            fail(f"duplicate evaluation cell across selected runs: {key}")
        cell_by_key[key] = row
    if set(grouped_samples) != set(cell_by_key):
        fail("sample groups and cell summaries do not match")

    result: list[dict[str, Any]] = []
    for key in sorted(grouped_samples, key=lambda item: (item[2], item[1], item[0], item[4], BASELINE_ORDER.get(item[3], 99))):
        experiment, family, scale_factor, baseline, concurrency = key
        rows = grouped_samples[key]
        cell = cell_by_key[key]
        latencies = [float(row["latency_ms"]) for row in rows]
        database = [float(row["database_ms"]) for row in rows if row.get("database_ms") is not None]
        receipts = [int(row["receipt_bytes"]) for row in rows if row.get("receipt_bytes") is not None]
        result.append(
            {
                "experiment": experiment,
                "family": family,
                "workload": family,
                "scale_factor": scale_factor,
                "baseline": baseline,
                "concurrency": concurrency,
                "measured_runs": len(rows),
                "p50_ms": rounded(percentile(latencies, 0.50)),
                "p95_ms": rounded(percentile(latencies, 0.95)),
                "p99_ms": rounded(percentile(latencies, 0.99)),
                "database_p50_ms": rounded(percentile(database, 0.50)),
                "throughput_qps": rounded(float(cell["throughput_qps"])),
                "measurement_seconds": rounded(float(cell["measurement_seconds"])),
                "cpu_seconds": rounded(sum_map(cell.get("cpu_seconds"))),
                "peak_memory_bytes": max_map(cell.get("peak_memory_bytes")),
                "control_transactions": cell.get("control_transactions"),
                "receipt_storage_bytes": cell.get("receipt_storage_bytes"),
                "receipt_mean_bytes": rounded(sum(receipts) / len(receipts)) if receipts else None,
                "component_ms": cell.get("component_ms") or {},
            }
        )
    return result


def write_summary_csv(path: pathlib.Path, rows: list[dict[str, Any]]) -> None:
    fields = [
        "experiment", "family", "workload", "scale_factor", "baseline", "concurrency",
        "measured_runs", "p50_ms", "p95_ms", "p99_ms", "database_p50_ms",
        "throughput_qps", "measurement_seconds", "cpu_seconds", "peak_memory_bytes",
        "control_transactions", "receipt_storage_bytes", "receipt_mean_bytes", "component_ms_json",
    ]
    with path.open("w", encoding="utf-8", newline="") as destination:
        writer = csv.DictWriter(destination, fieldnames=fields, lineterminator="\n")
        writer.writeheader()
        for row in rows:
            output = {key: row.get(key) for key in fields}
            output["component_ms_json"] = json.dumps(row.get("component_ms", {}), sort_keys=True, separators=(",", ":"))
            writer.writerow(output)


def latex_escape(value: Any) -> str:
    text = str(value)
    replacements = {
        "\\": r"\textbackslash{}", "&": r"\&", "%": r"\%", "$": r"\$",
        "#": r"\#", "_": r"\_", "{": r"\{", "}": r"\}",
    }
    return "".join(replacements.get(character, character) for character in text)


def fmt(value: Any, digits: int = 2) -> str:
    if value is None:
        return "--"
    if isinstance(value, (float, int)):
        return f"{float(value):.{digits}f}"
    return str(value)


def write_latex(path: pathlib.Path, rows: list[dict[str, Any]]) -> None:
    lines = [
        "% Generated from raw evaluation JSON; do not edit.",
        r"\begin{tabular}{llrrrrr}",
        r"\hline",
        r"Workload & Baseline & C & $n$ & p50 ms & p95 ms & QPS \\",
        r"\hline",
    ]
    for row in rows:
        workload = f"{row['family'].upper()} SF{row['scale_factor']}"
        lines.append(
            f"{latex_escape(workload)} & {latex_escape(BASELINE_LABEL[row['baseline']])} & "
            f"{row['concurrency']} & {row['measured_runs']} & {fmt(row['p50_ms'])} & "
            f"{fmt(row['p95_ms'])} & {fmt(row['throughput_qps'])} \\\\"
        )
    lines.extend([r"\hline", r"\end{tabular}", ""])
    path.write_text("\n".join(lines), encoding="utf-8")


def write_bar_svg(path: pathlib.Path, rows: list[dict[str, Any]], field: str, title: str, unit: str) -> None:
    width = 1120
    row_height = 24
    top = 72
    bottom = 48
    label_width = 330
    plot_width = width - label_width - 90
    height = top + bottom + row_height * len(rows)
    maximum = max(float(row[field] or 0.0) for row in rows) if rows else 0.0
    if maximum <= 0:
        maximum = 1.0
    lines = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}">',
        '<rect width="100%" height="100%" fill="white"/>',
        f'<text x="20" y="30" font-family="sans-serif" font-size="18">{html.escape(title)}</text>',
        f'<text x="20" y="52" font-family="sans-serif" font-size="11" fill="#444">Generated from completed raw runs; {html.escape(unit)}</text>',
    ]
    for index, row in enumerate(rows):
        y = top + index * row_height
        label = f"{row['family'].upper()} SF{row['scale_factor']} c={row['concurrency']} {BASELINE_LABEL[row['baseline']]}"
        value = float(row[field] or 0.0)
        bar_width = value / maximum * plot_width
        color = COLORS[row["baseline"]]
        lines.append(f'<text x="12" y="{y + 15}" font-family="sans-serif" font-size="11">{html.escape(label)}</text>')
        lines.append(f'<rect x="{label_width}" y="{y + 3}" width="{bar_width:.3f}" height="15" fill="{color}"/>')
        lines.append(f'<text x="{label_width + bar_width + 6:.3f}" y="{y + 15}" font-family="monospace" font-size="10">{value:.3f}</text>')
    lines.append("</svg>")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def load_formal(root: pathlib.Path, provenance: list[pathlib.Path]) -> dict[str, Any]:
    path = root / "formal" / "results" / "tlc.json"
    if not path.is_file():
        return {"status": "not_run", "note": "Run make formal to create a machine-readable TLC result."}
    value = read_json(path)
    provenance.append(path)
    for field in ("model", "config", "raw_log"):
        relative_path = value.get(field)
        if relative_path:
            referenced = root / str(relative_path)
            if not referenced.is_file():
                fail(f"formal result references missing {field}: {referenced}")
            provenance.append(referenced)
    return value


def load_security(root: pathlib.Path, provenance: list[pathlib.Path]) -> dict[str, Any]:
    result_path = root / "evaluation" / "security" / "results.json"
    if result_path.is_file():
        verifier_path = root / "evaluation" / "security" / "verify.py"
        spec = importlib.util.spec_from_file_location("taskgate_security_verify", verifier_path)
        if spec is None or spec.loader is None:
            fail(f"cannot load security verifier: {verifier_path}")
        verifier = importlib.util.module_from_spec(spec)
        try:
            spec.loader.exec_module(verifier)
            value, evidence_paths = verifier.verify_results_file(root, result_path)
        except Exception as exc:  # The verifier supplies the actionable evidence error.
            fail(f"security result verification failed: {exc}")
        provenance.extend([result_path, *evidence_paths])
        return value
    corpus_path = root / "evaluation" / "attacks" / "corpus.json"
    prompt_path = root / "evaluation" / "attacks" / "prompt-injection.json"
    corpus = read_json(corpus_path)
    prompts = read_json(prompt_path)
    provenance.extend([corpus_path, prompt_path])
    return {
        "status": "not_measured",
        "corpus_cases_defined": len(corpus.get("cases", [])),
        "prompt_injection_boundaries_defined": len(prompts.get("cases", [])),
        "note": "Attack and prompt-boundary corpora exist, but no verified machine-readable security run was selected.",
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default="/workspace", type=pathlib.Path)
    parser.add_argument("--raw-root", default="/workspace/evaluation/raw", type=pathlib.Path)
    parser.add_argument("--output", default="/workspace/evaluation/generated", type=pathlib.Path)
    parser.add_argument("--run-dir", action="append", default=[], type=pathlib.Path)
    parser.add_argument("--allow-empty", action="store_true")
    args = parser.parse_args()

    root = args.root.resolve()
    run_dirs = discover_runs(args.raw_root, args.run_dir, args.allow_empty)
    if run_dirs:
        samples, cells, raw_inputs, metadata_rows = load_measurements(root, run_dirs)
        summary = build_summary(samples, cells)
        if not summary:
            fail("selected runs contain no measured samples")
    else:
        raw_inputs, metadata_rows, summary = [], [], []

    output = args.output
    output.mkdir(parents=True, exist_ok=True)
    provenance_paths = list(raw_inputs)
    formal = load_formal(root, provenance_paths)
    security = load_security(root, provenance_paths)
    provenance = [
        {"path": relative(path, root), "sha256": sha256(path)}
        for path in sorted(set(provenance_paths), key=lambda value: relative(value, root))
    ]
    generated_at = max((str(row.get("finished_at", "")) for row in metadata_rows), default=str(formal.get("checked_at", "")))
    selected_modes = {str(row.get("mode", "")) for row in metadata_rows}
    if summary and selected_modes == {"full"}:
        performance_status = "complete"
    elif summary and selected_modes == {"smoke"}:
        performance_status = "smoke"
    else:
        performance_status = "not_measured"
    paper_results = {
        "schema_version": 1,
        "generated_at": generated_at,
        "provenance": {
            "raw_inputs": provenance,
            "run_ids": sorted(str(row["run_id"]) for row in metadata_rows),
            "campaign_ids": sorted({str(row["campaign_id"]) for row in metadata_rows if row.get("campaign_id")}),
            "percentile_method": "Hyndman-Fan type 7",
        },
        "formal": formal,
        "security": security,
        "performance": {"status": performance_status, "summary": summary},
    }

    (output / "summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    write_summary_csv(output / "summary.csv", summary)
    write_latex(output / "performance-table.tex", summary)
    write_bar_svg(output / "latency-p95.svg", summary, "p95_ms", "TaskGate evaluation: p95 latency", "milliseconds")
    write_bar_svg(output / "throughput.svg", summary, "throughput_qps", "TaskGate evaluation: throughput", "queries/second")
    (output / "paper-results.json").write_text(json.dumps(paper_results, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    if run_dirs:
        print("selected runs:")
        for directory in run_dirs:
            print(f"  {directory}")
    else:
        print("no completed raw run selected; wrote an honest not_measured performance object")
    print(f"ok - generated {output / 'paper-results.json'}")


if __name__ == "__main__":
    main()
