#!/usr/bin/env python3
"""Independent, fail-closed validation for the combined RQ5 evidence.

The offline half is delegated to the sealed-pack ``validate_pack`` API.  This
module independently validates the source-controlled online descriptor pack,
binds it to the same 345,000-row dataset, derives the five transition checks,
and computes paper-facing timing summaries with Hyndman--Fan Type 7 quantiles.

Offline and online compilation intentionally use different schema identities:
the offline campaign pins its candidate field schema, whereas the online run
pins live ``pg_get_viewdef`` attestations.  Therefore the cross-run contract is
the exact dataset (row count, generator, config, and dataset manifest), not
artifact-byte equality.  Every artifact identity is nevertheless compared and
reported, and the online approved inputs, Catalogs, and bundle manifests are
re-hashed from the compact online evidence directory.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import math
import os
import pathlib
import re
import stat
import statistics
import sys
from types import ModuleType
from typing import Any, Iterable


HERE = pathlib.Path(__file__).resolve().parent
REPOSITORY_ROOT = HERE.parents[1]
OFFLINE_VALIDATOR = (
    REPOSITORY_ROOT / "evaluation/daily-publication/evidence/validate.py"
)
DEFAULT_OFFLINE_PACK = (
    REPOSITORY_ROOT
    / "evaluation/daily-publication/evidence/scale-20260730-final3"
)
DEFAULT_ONLINE_EVIDENCE = (
    REPOSITORY_ROOT
    / "evaluation/daily-publication-online/evidence/scale-20260730-final"
    / "online-evidence.json"
)

SCHEMA = "taskgate-rq5-paper-evidence-v1"
ONLINE_SCHEMA = "taskgate-daily-publication-online-evidence-v1"
PREPARATION_SCHEMA = "taskgate-daily-publication-online-preparation-v1"
ROUTING_MODEL = "approval_time_version_routed_retained_instances"
ONLINE_MEASUREMENT_BOUNDARY = (
    "experiment_only_router_over_four_retained_catalog_bound_gateway_services; "
    "excludes offline build_verify_activate and production routing"
)
ROWS = 345_000
DAYS = ("day0", "day1", "day2", "day3")
CONDITIONS = (
    "old_task_returns_old_data",
    "new_task_sees_new_data",
    "old_task_ledger_unchanged_by_switch",
    "new_publication_misses_old_cache",
    "delegated_child_uses_root_publication",
)
PUBLICATION_DIGEST_FIELDS = (
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
CROSS_ARTIFACT_FIELDS = (
    "approved_input_sha256",
    "bundle_manifest_sha256",
    "publication_manifest_digest",
    "dictionary_digest",
    "sidecar_digest",
    "schema_digest",
    "hot_artifact_sha256",
    "cold_artifact_sha256",
    "sidecar_artifact_sha256",
)
HEX64 = re.compile(r"[0-9a-f]{64}\Z")
TASK_ID = re.compile(r"task_[0-9a-f]{32}\Z")
MAX_JSON_BYTES = 16 << 20
MAX_CATALOG_BYTES = 4 << 20

EXPECTED_FIELDS = [
    {"name": "dataset_partition", "canonical_field_id": "dataset_partition", "sql_type": "smallint"},
    {"name": "dataset_partition", "canonical_field_id": "daily_lineitem.dataset_partition", "sql_type": "smallint"},
    {"name": "l_orderkey", "canonical_field_id": "l_orderkey", "sql_type": "bigint"},
    {"name": "l_orderkey", "canonical_field_id": "daily_lineitem.l_orderkey", "sql_type": "bigint"},
    {"name": "l_linenumber", "canonical_field_id": "l_linenumber", "sql_type": "integer"},
    {"name": "l_linenumber", "canonical_field_id": "daily_lineitem.l_linenumber", "sql_type": "integer"},
    {"name": "l_extendedprice", "canonical_field_id": "l_extendedprice", "sql_type": "numeric"},
    {"name": "l_extendedprice", "canonical_field_id": "daily_lineitem.l_extendedprice", "sql_type": "numeric"},
]
EXPECTED_SEGMENTS = (
    "cell:daily_lineitem.dataset_partition",
    "cell:daily_lineitem.l_extendedprice",
    "cell:daily_lineitem.l_linenumber",
    "cell:daily_lineitem.l_orderkey",
    "cell:dataset_partition",
    "cell:l_extendedprice",
    "cell:l_linenumber",
    "cell:l_orderkey",
    "row",
)


class EvidenceError(ValueError):
    """Raised when combined RQ5 evidence is absent or inconsistent."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise EvidenceError("RQ5 paper evidence: " + message)


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            raise EvidenceError(f"RQ5 paper evidence: duplicate JSON key {key!r}")
        value[key] = item
    return value


def _reject_nonfinite(value: str) -> Any:
    raise EvidenceError(f"RQ5 paper evidence: non-finite JSON number {value!r}")


def _read_regular(path: pathlib.Path, maximum: int) -> bytes:
    try:
        before = os.lstat(path)
    except OSError as exc:
        raise EvidenceError(f"RQ5 paper evidence: cannot stat {path}: {exc}") from exc
    _require(
        stat.S_ISREG(before.st_mode) and not stat.S_ISLNK(before.st_mode),
        f"{path} must be a regular non-symlink file",
    )
    _require(0 < before.st_size <= maximum, f"{path} has an invalid byte size")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "rb") as source:
            opened = os.fstat(source.fileno())
            raw = source.read(maximum + 1)
            after = os.fstat(source.fileno())
        after_path = os.lstat(path)
    except OSError as exc:
        raise EvidenceError(f"RQ5 paper evidence: cannot read {path}: {exc}") from exc
    identity = lambda item: (item.st_dev, item.st_ino, item.st_size, item.st_mtime_ns)
    _require(
        identity(before) == identity(opened) == identity(after) == identity(after_path),
        f"{path} changed while being read",
    )
    _require(len(raw) == before.st_size and len(raw) <= maximum, f"{path} changed size")
    return raw


def _load_json(path: pathlib.Path, maximum: int = MAX_JSON_BYTES) -> tuple[dict[str, Any], bytes]:
    raw = _read_regular(path, maximum)
    try:
        value = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_reject_duplicate_pairs,
            parse_constant=_reject_nonfinite,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise EvidenceError(f"RQ5 paper evidence: invalid JSON in {path}: {exc}") from exc
    _require(isinstance(value, dict), f"{path} must contain one JSON object")
    return value, raw


def _sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _digest(value: Any, label: str) -> str:
    _require(
        isinstance(value, str) and HEX64.fullmatch(value) is not None,
        f"{label} must be a lowercase SHA-256",
    )
    return value


def _integer(value: Any, label: str, *, expected: int | None = None,
             minimum: int | None = None) -> int:
    _require(isinstance(value, int) and not isinstance(value, bool), f"{label} must be an integer")
    if expected is not None:
        _require(value == expected, f"{label} must equal {expected}")
    if minimum is not None:
        _require(value >= minimum, f"{label} must be at least {minimum}")
    return value


def _positive_number(value: Any, label: str) -> float:
    _require(
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(float(value))
        and float(value) > 0,
        f"{label} must be positive finite numeric evidence",
    )
    return float(value)


def _exact_fields(value: Any, expected: set[str], label: str) -> dict[str, Any]:
    _require(isinstance(value, dict), f"{label} must be an object")
    _require(set(value) == expected, f"{label} fields differ: {sorted(set(value) ^ expected)}")
    return value


def _type7(values: list[float], probability: float) -> float:
    _require(bool(values), "cannot summarize an empty measurement")
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * probability
    lower = math.floor(position)
    fraction = position - lower
    return ordered[lower] + fraction * (ordered[min(lower + 1, len(ordered) - 1)] - ordered[lower])


def _summary(values: Iterable[float]) -> dict[str, Any]:
    items = [float(value) for value in values]
    _require(bool(items) and all(math.isfinite(value) and value >= 0 for value in items),
             "timing summary contains no values or invalid values")
    return {
        "count": len(items),
        "min": min(items),
        "p50": _type7(items, 0.50),
        "p95": _type7(items, 0.95),
        "max": max(items),
        "mean": statistics.fmean(items),
        "quantile_method": "Hyndman-Fan Type 7",
    }


def _offline_module() -> ModuleType:
    spec = importlib.util.spec_from_file_location("taskgate_rq5_offline_validator", OFFLINE_VALIDATOR)
    _require(spec is not None and spec.loader is not None,
             f"cannot load offline validator {OFFLINE_VALIDATOR}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _validate_offline_pack(pack: pathlib.Path) -> tuple[dict[str, Any], ModuleType]:
    validator = _offline_module()
    try:
        canonical = validator.validate_pack(pack)
    except validator.EvidenceError as exc:
        raise EvidenceError(f"RQ5 paper evidence: offline sealed pack failed: {exc}") from exc
    _require(canonical.get("offline_status") == "complete", "offline sealed pack is incomplete")
    _require(canonical.get("workload", {}).get("rows_per_publication") == ROWS,
             f"offline sealed pack must contain exactly {ROWS} rows per publication")
    return canonical, validator


def _offline_bindings(pack: pathlib.Path, validator: ModuleType) -> tuple[dict[str, str], dict[str, dict[str, str]]]:
    source, _ = _load_json(pack / "source-manifest.json")
    members = source.get("run_bound_members")
    _require(isinstance(members, dict), "offline source manifest lacks run-bound members")
    source_bindings = {
        "generator_sha256": _digest(
            members.get("evaluation/daily-publication/sql/05-generate-daily-data.sh", {}).get("sha256"),
            "offline generator SHA-256",
        ),
        "config_sha256": _digest(
            members.get("evaluation/daily-publication/config.json", {}).get("sha256"),
            "offline config SHA-256",
        ),
        "dataset_manifest_sha256": validator.file_sha256(pack / "dataset-manifest.json"),
    }
    publications: dict[str, dict[str, str]] = {}
    for day in DAYS:
        name = f"daily-lineitem-{day}-r{ROWS}"
        approved = pack / "approved-inputs" / f"{day}.json"
        bundle_path = pack / "calibration" / day / "artifacts" / name / f"{name}.bundle.json"
        bundle, bundle_raw = _load_json(bundle_path)
        dictionary = bundle.get("dictionary_manifest")
        _require(isinstance(dictionary, dict), f"offline {day} dictionary manifest is absent")
        publications[day] = {
            "approved_input_sha256": validator.file_sha256(approved),
            "bundle_manifest_sha256": _sha256(bundle_raw),
            "publication_manifest_digest": _digest(bundle.get("manifest_digest"), f"offline {day} manifest"),
            "dictionary_digest": _digest(dictionary.get("dictionary_digest"), f"offline {day} dictionary"),
            "sidecar_digest": _digest(dictionary.get("sidecar_digest"), f"offline {day} sidecar"),
            "schema_digest": _digest(dictionary.get("schema_digest"), f"offline {day} schema"),
            "hot_artifact_sha256": _digest(bundle.get("hot", {}).get("sha256"), f"offline {day} HOT"),
            "cold_artifact_sha256": _digest(bundle.get("cold", {}).get("sha256"), f"offline {day} COLD"),
            "sidecar_artifact_sha256": _digest(bundle.get("sidecar", {}).get("sha256"), f"offline {day} sidecar artifact"),
        }
    return source_bindings, publications


def _validate_fixture(value: Any, source_bindings: dict[str, str]) -> list[dict[str, Any]]:
    fixture = _exact_fields(value, {
        "fixture_class", "rows_per_publication", "generator_sha256", "config_sha256",
        "dataset_manifest_sha256", "publications",
    }, "online fixture")
    _require(fixture["fixture_class"] == "correctness_fixture",
             "online fixture_class must be correctness_fixture")
    _integer(fixture["rows_per_publication"], "online fixture rows_per_publication", expected=ROWS)
    for field in ("generator_sha256", "config_sha256", "dataset_manifest_sha256"):
        actual = _digest(fixture[field], f"online fixture {field}")
        _require(actual == source_bindings[field], f"online fixture {field} differs from offline sealed pack")

    values = fixture["publications"]
    _require(isinstance(values, list) and len(values) == len(DAYS),
             "online fixture must contain exactly four ordered publications")
    publications: list[dict[str, Any]] = []
    expected_fields = {"day", "publication_name", "row_count", *PUBLICATION_DIGEST_FIELDS}
    for index, value in enumerate(values):
        day = DAYS[index]
        publication = _exact_fields(value, expected_fields, f"online fixture publication {day}")
        name = f"daily-lineitem-{day}-r{ROWS}"
        _require(publication["day"] == day and publication["publication_name"] == name,
                 f"online fixture publication {day} identity differs")
        _integer(publication["row_count"], f"online fixture {day} row_count", expected=ROWS)
        for field in PUBLICATION_DIGEST_FIELDS:
            _digest(publication[field], f"online fixture {day}.{field}")
        publications.append(publication)
    for field in ("catalog_sha256", "publication_manifest_digest", "direct_result_sha256"):
        _require(len({value[field] for value in publications}) == len(DAYS),
                 f"online fixture {field} must distinguish all four publications")
    return publications


def _validate_approved_input(path: pathlib.Path, day: str, fixture: dict[str, Any],
                             bundle: dict[str, Any]) -> None:
    approved, raw = _load_json(path)
    _require(_sha256(raw) == fixture["approved_input_sha256"],
             f"online {day} approved input SHA-256 differs")
    _exact_fields(approved, {
        "version", "publication_name", "catalog_source", "source_relation",
        "ordinal_sidecar", "entity_key_fields", "snapshot", "expected_digests",
    }, f"online {day} approved input")
    name = fixture["publication_name"]
    _require(
        approved["version"] == "taskgate-snapshot-index-input-v1"
        and approved["publication_name"] == name
        and approved["catalog_source"] == "daily_reporting"
        and approved["source_relation"] == f"reporting.daily_lineitem_{day}"
        and approved["ordinal_sidecar"] == f"taskgate_ordinal.daily_lineitem_{day}_r{ROWS}"
        and approved["entity_key_fields"] == ["l_orderkey", "l_linenumber"],
        f"online {day} approved input identity differs",
    )
    snapshot = _exact_fields(approved["snapshot"], {
        "source_id", "source_namespace", "snapshot", "schema_digest", "fields", "rows",
    }, f"online {day} approved snapshot")
    _require(
        snapshot["source_id"] == "taskgate-eval-daily-publication"
        and snapshot["source_namespace"] == "evaluation.daily_lineitem"
        and snapshot["snapshot"] == f"rq5-daily-lineitem-{day}-rows-{ROWS}"
        and snapshot["schema_digest"] == fixture["schema_digest"]
        and snapshot["fields"] == EXPECTED_FIELDS
        and snapshot["rows"] == [],
        f"online {day} approved snapshot differs",
    )
    expected = _exact_fields(approved["expected_digests"], {
        "sidecar_digest", "dictionary_digest", "manifest_digest",
        "cold_payload_digest", "hot_index_digest",
    }, f"online {day} expected digests")
    dictionary = bundle["dictionary_manifest"]
    _require(
        expected == {
            "sidecar_digest": dictionary["sidecar_digest"],
            "dictionary_digest": dictionary["dictionary_digest"],
            "manifest_digest": bundle["manifest_digest"],
            "cold_payload_digest": dictionary["cold_payload_digest"],
            "hot_index_digest": dictionary["hot_index_digest"],
        },
        f"online {day} approved input does not bind its bundle",
    )


def _validate_bundle(path: pathlib.Path, day: str, fixture: dict[str, Any]) -> dict[str, Any]:
    bundle, raw = _load_json(path)
    _require(_sha256(raw) == fixture["bundle_manifest_sha256"],
             f"online {day} bundle manifest SHA-256 differs")
    _exact_fields(bundle, {
        "version", "publication_name", "catalog_source", "ordinal_sidecar",
        "manifest_digest", "dictionary_manifest", "row_count", "hot", "cold", "sidecar",
    }, f"online {day} bundle")
    name = fixture["publication_name"]
    _require(
        bundle["version"] == "taskgate-snapshot-index-bundle-v1"
        and bundle["publication_name"] == name
        and bundle["catalog_source"] == "daily_reporting"
        and bundle["ordinal_sidecar"] == f"taskgate_ordinal.daily_lineitem_{day}_r{ROWS}"
        and bundle["manifest_digest"] == fixture["publication_manifest_digest"],
        f"online {day} bundle identity differs",
    )
    _integer(bundle["row_count"], f"online {day} bundle row_count", expected=ROWS)

    dictionary = _exact_fields(bundle["dictionary_manifest"], {
        "version", "source_id", "source_namespace", "snapshot", "schema_digest",
        "dictionary_digest", "sidecar_digest", "cold_payload_digest",
        "hot_index_digest", "segments",
    }, f"online {day} dictionary manifest")
    _require(
        dictionary["version"] == "taskgate-ordinal-dictionary-v1"
        and dictionary["source_id"] == "taskgate-eval-daily-publication"
        and dictionary["source_namespace"] == "evaluation.daily_lineitem"
        and dictionary["snapshot"] == f"rq5-daily-lineitem-{day}-rows-{ROWS}"
        and dictionary["schema_digest"] == fixture["schema_digest"]
        and dictionary["dictionary_digest"] == fixture["dictionary_digest"]
        and dictionary["sidecar_digest"] == fixture["sidecar_digest"],
        f"online {day} dictionary identity differs",
    )
    for field in ("schema_digest", "dictionary_digest", "sidecar_digest",
                  "cold_payload_digest", "hot_index_digest"):
        _digest(dictionary[field], f"online {day} dictionary.{field}")

    segments = dictionary["segments"]
    _require(isinstance(segments, list) and len(segments) == len(EXPECTED_SEGMENTS),
             f"online {day} dictionary must contain nine segments")
    for index, expected_id in enumerate(EXPECTED_SEGMENTS):
        segment = segments[index]
        expected_keys = {"id", "kind", "shard", "fact_count", "hashes_digest", "payloads_digest"}
        if expected_id != "row":
            expected_keys.add("field")
        _exact_fields(segment, expected_keys, f"online {day} segment {expected_id}")
        _require(segment["id"] == expected_id and segment["shard"] == 0,
                 f"online {day} segment {expected_id} identity differs")
        expected_kind = "base-row" if expected_id == "row" else "base-cell"
        _require(segment["kind"] == expected_kind,
                 f"online {day} segment {expected_id} kind differs")
        if expected_id != "row":
            _require(segment["field"] == expected_id.removeprefix("cell:"),
                     f"online {day} segment {expected_id} field differs")
        _integer(segment["fact_count"], f"online {day} segment {expected_id} fact_count", expected=ROWS)
        _digest(segment["hashes_digest"], f"online {day} segment {expected_id} hashes")
        _digest(segment["payloads_digest"], f"online {day} segment {expected_id} payloads")

    suffixes = {"hot": ".hot.tgord", "cold": ".cold.tgord", "sidecar": ".sidecar.ndjson"}
    fixture_fields = {
        "hot": "hot_artifact_sha256",
        "cold": "cold_artifact_sha256",
        "sidecar": "sidecar_artifact_sha256",
    }
    for role in ("hot", "cold", "sidecar"):
        descriptor = _exact_fields(bundle[role], {"name", "sha256", "bytes"},
                                   f"online {day} {role} descriptor")
        _require(descriptor["name"] == name + suffixes[role],
                 f"online {day} {role} artifact name differs")
        _require(_digest(descriptor["sha256"], f"online {day} {role} descriptor")
                 == fixture[fixture_fields[role]],
                 f"online {day} {role} descriptor SHA-256 differs from fixture")
        _integer(descriptor["bytes"], f"online {day} {role} descriptor bytes", minimum=1)
    return bundle


def _validate_preparation(path: pathlib.Path, publications: list[dict[str, Any]]) -> None:
    preparation, _ = _load_json(path)
    _exact_fields(preparation, {"schema_version", "publications"}, "online preparation")
    _require(preparation["schema_version"] == PREPARATION_SCHEMA,
             "online preparation schema differs")
    entries = preparation["publications"]
    _require(isinstance(entries, list) and len(entries) == len(DAYS),
             "online preparation must contain four publications")
    expected_fields = {
        "day", "publication_name", "rows", "schema_digest", "manifest_digest",
        "dictionary_digest", "sidecar_digest", "input_sha256",
    }
    for day, fixture, value in zip(DAYS, publications, entries, strict=True):
        entry = _exact_fields(value, expected_fields, f"online preparation {day}")
        _require(
            entry == {
                "day": day,
                "publication_name": fixture["publication_name"],
                "rows": ROWS,
                "schema_digest": fixture["schema_digest"],
                "manifest_digest": fixture["publication_manifest_digest"],
                "dictionary_digest": fixture["dictionary_digest"],
                "sidecar_digest": fixture["sidecar_digest"],
                "input_sha256": fixture["approved_input_sha256"],
            },
            f"online preparation {day} differs from fixture bindings",
        )


def _validate_online_descriptors(root: pathlib.Path, publications: list[dict[str, Any]],
                                 dataset_manifest_sha256: str) -> None:
    dataset_raw = _read_regular(root / "dataset-manifest.json", MAX_JSON_BYTES)
    _require(_sha256(dataset_raw) == dataset_manifest_sha256,
             "retained online dataset manifest SHA-256 differs")
    _validate_preparation(root / "preparation.json", publications)
    for day, fixture in zip(DAYS, publications, strict=True):
        catalog_raw = _read_regular(root / "catalogs" / f"{day}.yaml", MAX_CATALOG_BYTES)
        _require(_sha256(catalog_raw) == fixture["catalog_sha256"],
                 f"online {day} Catalog SHA-256 differs")
        name = fixture["publication_name"]
        bundle_path = root / "artifacts" / name / f"{name}.bundle.json"
        bundle = _validate_bundle(bundle_path, day, fixture)
        _validate_approved_input(root / "approved-inputs" / f"{day}.json", day, fixture, bundle)


def _validate_transition(value: Any, index: int, publications: list[dict[str, Any]]) -> tuple[dict[str, bool], dict[str, float], str, str]:
    source, target = DAYS[index], DAYS[index + 1]
    transition = _exact_fields(value, {
        "from", "to", "switch_wall_ms", "first_query_wall_ms", "replay_wall_ms",
        "old_task", "new_task", "old_ledger", "cache", "delegation",
    }, f"online transition {source}->{target}")
    _require(transition["from"] == source and transition["to"] == target,
             f"online transition {index} must be {source}->{target}")
    timings = {
        "switch_ms": _positive_number(transition["switch_wall_ms"], f"{source}->{target} switch_wall_ms"),
        "first_query_ms": _positive_number(transition["first_query_wall_ms"], f"{source}->{target} first_query_wall_ms"),
        "replay_ms": _positive_number(transition["replay_wall_ms"], f"{source}->{target} replay_wall_ms"),
    }
    old = _exact_fields(transition["old_task"], {
        "publication_digest_before", "publication_digest_after", "expected_publication_digest",
        "result_sha256_before", "result_sha256_after", "expected_result_sha256",
    }, f"{source}->{target} old_task")
    new = _exact_fields(transition["new_task"], {
        "publication_digest", "expected_publication_digest", "result_sha256", "expected_result_sha256",
    }, f"{source}->{target} new_task")
    ledger = _exact_fields(transition["old_ledger"], {
        "before_switch_sha256", "after_switch_sha256",
    }, f"{source}->{target} old_ledger")
    cache = _exact_fields(transition["cache"], {
        "old_cache_key_sha256", "first_new_cache_key_sha256", "first_new_semantic_replay",
        "replay_new_cache_key_sha256", "replay_new_semantic_replay",
    }, f"{source}->{target} cache")
    delegation = _exact_fields(transition["delegation"], {
        "root_task_id", "child_root_task_id", "child_parent_task_id",
        "root_publication_digest", "child_publication_digest",
    }, f"{source}->{target} delegation")
    for owner, fields in (
        (old, tuple(old)), (new, tuple(new)), (ledger, tuple(ledger)),
        (cache, ("old_cache_key_sha256", "first_new_cache_key_sha256", "replay_new_cache_key_sha256")),
        (delegation, ("root_publication_digest", "child_publication_digest")),
    ):
        for field in fields:
            _digest(owner[field], f"{source}->{target}.{field}")
    for field in ("first_new_semantic_replay", "replay_new_semantic_replay"):
        _require(type(cache[field]) is bool, f"{source}->{target}.{field} must be boolean")
    for field in ("root_task_id", "child_root_task_id", "child_parent_task_id"):
        _require(isinstance(delegation[field], str) and TASK_ID.fullmatch(delegation[field]) is not None,
                 f"{source}->{target}.{field} must be a task ID")

    source_fixture, target_fixture = publications[index], publications[index + 1]
    checks = {
        "old_task_returns_old_data": (
            old["publication_digest_before"] == old["publication_digest_after"]
            == old["expected_publication_digest"] == source_fixture["publication_manifest_digest"]
            and old["result_sha256_before"] == old["result_sha256_after"]
            == old["expected_result_sha256"] == source_fixture["direct_result_sha256"]
        ),
        "new_task_sees_new_data": (
            new["publication_digest"] == new["expected_publication_digest"]
            == target_fixture["publication_manifest_digest"]
            and new["result_sha256"] == new["expected_result_sha256"]
            == target_fixture["direct_result_sha256"]
            and new["result_sha256"] != old["result_sha256_after"]
        ),
        "old_task_ledger_unchanged_by_switch": (
            ledger["before_switch_sha256"] == ledger["after_switch_sha256"]
        ),
        "new_publication_misses_old_cache": (
            cache["old_cache_key_sha256"] != cache["first_new_cache_key_sha256"]
            and cache["first_new_semantic_replay"] is False
            and cache["first_new_cache_key_sha256"] == cache["replay_new_cache_key_sha256"]
            and cache["replay_new_semantic_replay"] is True
        ),
        "delegated_child_uses_root_publication": (
            delegation["root_task_id"] == delegation["child_root_task_id"]
            == delegation["child_parent_task_id"]
            and delegation["root_publication_digest"] == delegation["child_publication_digest"]
            == new["expected_publication_digest"] == target_fixture["publication_manifest_digest"]
        ),
    }
    for condition, passed in checks.items():
        _require(passed, f"{source}->{target} failed correctness condition {condition}")
    return checks, timings, cache["old_cache_key_sha256"], cache["first_new_cache_key_sha256"]


def _online_summary(evidence_path: pathlib.Path, source_bindings: dict[str, str],
                    offline_publications: dict[str, dict[str, str]]) -> dict[str, Any]:
    evidence, raw = _load_json(evidence_path)
    _exact_fields(evidence, {
        "schema_version", "routing_model", "rows_per_publication",
        "measurement_boundary", "fixture", "transitions",
    }, "online evidence")
    _require(evidence["schema_version"] == ONLINE_SCHEMA, "online evidence schema differs")
    _require(evidence["routing_model"] == ROUTING_MODEL, "online routing model differs")
    _integer(evidence["rows_per_publication"], "online rows_per_publication", expected=ROWS)
    _require(evidence["measurement_boundary"] == ONLINE_MEASUREMENT_BOUNDARY,
             "online measurement boundary differs")
    publications = _validate_fixture(evidence["fixture"], source_bindings)
    _validate_online_descriptors(
        evidence_path.parent, publications, source_bindings["dataset_manifest_sha256"]
    )

    transitions = evidence["transitions"]
    _require(isinstance(transitions, list) and len(transitions) == 3,
             "online evidence must contain exactly three transitions")
    timing_values = {"switch": [], "first_query": [], "replay": []}
    derived: list[dict[str, Any]] = []
    old_cache_keys: list[str] = []
    new_cache_keys: list[str] = []
    root_task_ids: list[str] = []
    condition_counts = {condition: 0 for condition in CONDITIONS}
    for index, transition in enumerate(transitions):
        checks, timings, old_cache, new_cache = _validate_transition(transition, index, publications)
        for condition in CONDITIONS:
            condition_counts[condition] += int(checks[condition])
        timing_values["switch"].append(timings["switch_ms"])
        timing_values["first_query"].append(timings["first_query_ms"])
        timing_values["replay"].append(timings["replay_ms"])
        old_cache_keys.append(old_cache)
        new_cache_keys.append(new_cache)
        root_task_ids.append(transition["delegation"]["root_task_id"])
        derived.append({
            "from": DAYS[index], "to": DAYS[index + 1],
            "timing_ms": timings, "checks": checks,
        })
    _require(old_cache_keys[1:] == new_cache_keys[:-1],
             "online transition cache keys do not form one day0->day3 chain")
    _require(len(set([old_cache_keys[0], *new_cache_keys])) == len(DAYS),
             "online daily cache identities are not distinct")
    _require(len(set(root_task_ids)) == len(root_task_ids),
             "online transitions reuse a new root task ID")

    comparisons: dict[str, dict[str, Any]] = {}
    for fixture in publications:
        day = fixture["day"]
        per_day: dict[str, Any] = {}
        for field in CROSS_ARTIFACT_FIELDS:
            offline = offline_publications[day][field]
            online = fixture[field]
            per_day[field] = {
                "matches": online == offline,
                "offline_sha256": offline,
                "online_sha256": online,
            }
        matching = [field for field in CROSS_ARTIFACT_FIELDS if per_day[field]["matches"]]
        differing = [field for field in CROSS_ARTIFACT_FIELDS if not per_day[field]["matches"]]
        _require(bool(differing),
                 f"online {day} unexpectedly has no distinct attested artifact identity")
        comparisons[day] = {
            "matching_fields": matching,
            "differing_fields": differing,
            "fields": per_day,
        }

    return {
        "status": "complete",
        "evidence_sha256": _sha256(raw),
        "routing_model": ROUTING_MODEL,
        "measurement_boundary": ONLINE_MEASUREMENT_BOUNDARY,
        "rows_per_publication": ROWS,
        "transition_count": 3,
        "transitions": derived,
        "conditions": {
            condition: {"status": "pass", "pass_count": count, "required": 3}
            for condition, count in condition_counts.items()
        },
        "all_five_conditions_pass": all(count == 3 for count in condition_counts.values()),
        "timing_ms": {name: _summary(values) for name, values in timing_values.items()},
        "fixture_bindings": {
            "generator_sha256": evidence["fixture"]["generator_sha256"],
            "config_sha256": evidence["fixture"]["config_sha256"],
            "dataset_manifest_sha256": evidence["fixture"]["dataset_manifest_sha256"],
        },
        "artifact_identity_comparison": comparisons,
        "descriptor_audit_boundary": (
            "approved inputs, Catalog YAML, and bundle manifests are independently re-hashed; "
            "omitted HOT/COLD/sidecar payload bytes remain bound by retained bundle descriptors"
        ),
    }


def _offline_summary(canonical: dict[str, Any]) -> dict[str, Any]:
    changes = canonical["workload"]["changes_from_previous"]
    timing_values = {"build": [], "strict_verify": [], "activation": [], "cycle": []}
    days: dict[str, Any] = {}
    for value in canonical["days"]:
        day = value["id"]
        samples = value["samples"]
        day_values = {
            "build": [float(sample["wall_ms"]["build"]) for sample in samples],
            "strict_verify": [float(sample["wall_ms"]["strict_verify"]) for sample in samples],
            "activation": [float(sample["wall_ms"]["activation"]) for sample in samples],
            "cycle": [float(sample["cycle_ms"]) for sample in samples],
        }
        for phase, measurements in day_values.items():
            timing_values[phase].extend(measurements)
        days[day] = {
            "changes": changes.get(day, {"updated_rows": 0, "inserted_rows": 0, "deleted_rows": 0}),
            "measured_builds": len(samples),
            "build_ms": _summary(day_values["build"]),
            "strict_verify_ms": _summary(day_values["strict_verify"]),
            "activation_ms": _summary(day_values["activation"]),
            "cycle_ms": _summary(day_values["cycle"]),
            "artifact_bytes": value["summary"]["artifact_bytes"],
            "hot_artifact_bytes": value["summary"]["hot_artifact_bytes"],
            "bundle_manifest_sha256": value["bundle_manifest_sha256"],
        }
    metrics = canonical["metrics"]
    return {
        "status": "complete",
        "run_id": canonical["run_id"],
        "rows_per_publication": ROWS,
        "facts_per_publication": canonical["workload"]["facts_per_publication"],
        "publication_count": 4,
        "measured_builds_per_publication": canonical["workload"]["measured_builds_per_publication"],
        "days": days,
        "timing_ms": {name: _summary(values) for name, values in timing_values.items()},
        "metrics": {
            "maximum_cycle_ms": metrics["maximum_cycle_ms"],
            "maximum_build_ms": metrics["maximum_build_ms"],
            "maximum_strict_verify_ms": metrics["maximum_strict_verify_ms"],
            "maximum_activation_ms": metrics["maximum_activation_ms"],
            "maximum_builder_peak_rss_bytes": metrics["maximum_builder_peak_rss_bytes"],
            "phase_container_memory_limit_bytes": metrics["phase_container_memory_limit_bytes"],
            "artifact_bytes_by_day": metrics["artifact_bytes_by_day"],
            "hot_artifact_bytes_by_day": metrics["hot_artifact_bytes_by_day"],
        },
        "daily_cycle_gate": {
            "status": "pass" if metrics["maximum_cycle_ms"] <= 300_000 else "fail",
            "threshold_ms": 300_000,
            "maximum_cycle_ms": metrics["maximum_cycle_ms"],
        },
        "measurement_boundary": canonical["measurement_boundary"],
        "cache_policy": canonical["cache_policy"],
    }


def validate_rq5(
    offline_pack: pathlib.Path | str = DEFAULT_OFFLINE_PACK,
    online_evidence: pathlib.Path | str = DEFAULT_ONLINE_EVIDENCE,
) -> dict[str, Any]:
    """Validate both RQ5 halves and return canonical paper metrics.

    The function succeeds only when the sealed offline pack passes, the online
    evidence uses the exact 345,000-row dataset, every retained online
    descriptor is bound, and all five correctness conditions pass on each of
    the three ordered publication transitions.
    """
    pack = pathlib.Path(offline_pack).resolve()
    online_path = pathlib.Path(online_evidence).resolve()
    canonical, validator = _validate_offline_pack(pack)
    source_bindings, offline_publications = _offline_bindings(pack, validator)
    online = _online_summary(online_path, source_bindings, offline_publications)
    offline = _offline_summary(canonical)
    _require(offline["daily_cycle_gate"]["status"] == "pass",
             "offline daily-cycle gate failed")
    _require(online["all_five_conditions_pass"], "online five-condition gate failed")
    return {
        "schema_version": SCHEMA,
        "status": "complete",
        "rq": (
            "Can TaskGate safely refresh a daily reporting publication while preserving "
            "task binding, ledger isolation, and replay separation?"
        ),
        "dataset_relation": {
            "classification": "same_dataset_distinct_attested_artifacts",
            "rows_per_publication": ROWS,
            "generator_sha256": source_bindings["generator_sha256"],
            "config_sha256": source_bindings["config_sha256"],
            "dataset_manifest_sha256": source_bindings["dataset_manifest_sha256"],
            "artifact_identity_equality_required": False,
            "reason": (
                "offline compilation pins the candidate field schema; online compilation "
                "pins each retained live-view attestation before rebuilding"
            ),
        },
        "offline": offline,
        "online": online,
        "gates": {
            "same_345000_row_dataset": "pass",
            "three_offline_builds_per_publication": "pass",
            "daily_cycle_under_five_minutes": offline["daily_cycle_gate"]["status"],
            "three_online_transitions_measured": "pass",
            "all_five_correctness_conditions_on_every_transition": "pass",
        },
        "limitations": [
            "Offline phase times sum non-contiguous child-process intervals and exclude orchestration and calibration.",
            "Offline VmHWM is the direct builder process peak, not cgroup or full-system memory.",
            "Online switch timing measures an experiment-only in-process router, not a production deployment control plane.",
            "The compact online pack omits HOT/COLD/sidecar payload bytes; it validates their retained descriptors, not their bytes.",
            "Offline and online artifact hashes are compared but are not expected to match because their schema attestation paths differ.",
        ],
    }


def validate_rq5_evidence(
    offline_pack: pathlib.Path | str = DEFAULT_OFFLINE_PACK,
    online_evidence: pathlib.Path | str = DEFAULT_ONLINE_EVIDENCE,
) -> dict[str, Any]:
    """Long-form alias for :func:`validate_rq5`."""
    return validate_rq5(offline_pack, online_evidence)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--offline-pack", type=pathlib.Path, default=DEFAULT_OFFLINE_PACK)
    parser.add_argument("--online-evidence", type=pathlib.Path, default=DEFAULT_ONLINE_EVIDENCE)
    parser.add_argument("--json", action="store_true", help="print all canonical paper metrics")
    return parser


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    try:
        result = validate_rq5(arguments.offline_pack, arguments.online_evidence)
    except EvidenceError as exc:
        print(str(exc), file=sys.stderr)
        return 1
    if arguments.json:
        print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
    else:
        offline = result["offline"]
        online = result["online"]
        print(
            "RQ5 combined evidence valid: "
            f"rows={offline['rows_per_publication']} "
            f"builds={offline['measured_builds_per_publication']}x4 "
            f"transitions={online['transition_count']} "
            f"checks={len(CONDITIONS)}x{online['transition_count']} "
            f"max_cycle_ms={offline['metrics']['maximum_cycle_ms']:.6f}"
        )
    return 0


__all__ = [
    "DEFAULT_OFFLINE_PACK", "DEFAULT_ONLINE_EVIDENCE", "EvidenceError",
    "validate_rq5", "validate_rq5_evidence",
]


if __name__ == "__main__":
    raise SystemExit(main())
