#!/usr/bin/env python3
"""Validate the source-controlled RQ5 offline evidence pack.

The validator is intentionally independent of the campaign harness.  It treats
the retained phase reports, receipts, bundle manifests, inputs, source archive,
and environment record as primary evidence and recomputes the offline result.
Large HOT/COLD/sidecar payloads are not required; their retained descriptors
are validated, but their bytes cannot be re-hashed from a fresh clone.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import functools
import hashlib
import json
import math
import pathlib
import re
import statistics
import sys
import tarfile
from typing import Any, Iterable


SCHEMA = "taskgate-daily-publication-evidence-pack-v1"
SOURCE_SCHEMA = "taskgate-daily-publication-source-snapshot-v1"
ENVIRONMENT_SCHEMA = "taskgate-daily-publication-environment-v1"
OMISSION_SCHEMA = "taskgate-daily-publication-transport-omissions-v1"
CANONICAL_SCHEMA = "taskgate-daily-publication-canonical-offline-v1"
RESULT_SCHEMA = "taskgate-daily-publication-results-v1"
DATASET_SCHEMA = "taskgate-daily-publication-dataset-v1"
PHASE_SCHEMA = "taskgate-daily-publication-phase-v1"
RECEIPT_SCHEMA = "taskgate-snapshot-verification-receipt-v1"
BUNDLE_SCHEMA = "taskgate-snapshot-index-bundle-v1"
CONFIG_SCHEMA = "taskgate-daily-publication-config-v1"
INPUT_SET_SCHEMA = "taskgate-daily-publication-input-set-v1"
INPUT_SCHEMA = "taskgate-snapshot-index-input-v1"

RUN_ID = "scale-20260730-final3"
ROWS = 345_000
RUNS = 3
CYCLE_GATE_MS = 300_000
PHASE_MEMORY_LIMIT_BYTES = 6 * 1024**3
PRIOR_BUILDER_ENVELOPE_BYTES = 4 * 1024**3
TOTAL_ARTIFACT_LIMIT_BYTES = 2 * 1024**3
HOT_ARTIFACT_LIMIT_BYTES = 160 * 1024**2
DAYS = ("day0", "day1", "day2", "day3")
PHASES = ("build", "strict_verify", "activation")
MODES = {"build": "build", "strict_verify": "verify", "activation": "activate"}
MEASUREMENT_BOUNDARY = (
    "child process wall clock and /proc/<pid>/status VmHWM; "
    "excludes container startup and orchestration"
)
RSS_SCOPE = "root_process_vm_hwm_linux_procfs"
RECEIPT_DIGEST_DOMAIN = b"taskgate/snapshot-verification-receipt/v1\x00"
EMPTY_SHA256 = hashlib.sha256(b"").hexdigest()
DIGEST_RE = re.compile(r"[0-9a-f]{64}\Z")
MD5_RE = re.compile(r"[0-9a-f]{32}\Z")

HERE = pathlib.Path(__file__).resolve().parent
DEFAULT_PACK = HERE / RUN_ID


class EvidenceError(ValueError):
    """Raised when retained evidence is absent, malformed, or inconsistent."""


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            raise EvidenceError(f"duplicate JSON key {key!r}")
        value[key] = item
    return value


def _reject_nonfinite(value: str) -> Any:
    raise EvidenceError(f"non-finite JSON number {value!r}")


def load_json(path: pathlib.Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as source:
            value = json.load(
                source,
                object_pairs_hook=_reject_duplicate_pairs,
                parse_constant=_reject_nonfinite,
            )
    except (OSError, json.JSONDecodeError) as exc:
        raise EvidenceError(f"read {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise EvidenceError(f"{path} must contain one JSON object")
    return value


def canonical_bytes(value: Any) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")


def pretty_bytes(value: Any) -> bytes:
    return (
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    ).encode("utf-8")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def file_sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as source:
            for block in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(block)
    except OSError as exc:
        raise EvidenceError(f"hash {path}: {exc}") from exc
    return digest.hexdigest()


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise EvidenceError(message)


def _require_digest(value: Any, name: str) -> str:
    _require(isinstance(value, str) and DIGEST_RE.fullmatch(value) is not None,
             f"{name} must be a lowercase SHA-256")
    return value


def _require_positive(value: Any, name: str) -> float:
    _require(
        not isinstance(value, bool)
        and isinstance(value, (int, float))
        and math.isfinite(value)
        and value > 0,
        f"{name} must be positive and finite",
    )
    return float(value)


def _safe_relative(value: str, name: str) -> pathlib.PurePosixPath:
    path = pathlib.PurePosixPath(value)
    _require(
        value == path.as_posix()
        and not path.is_absolute()
        and value not in ("", ".")
        and ".." not in path.parts,
        f"{name} must be a normalized safe relative path",
    )
    return path


def _parse_time(value: Any, name: str) -> dt.datetime:
    _require(isinstance(value, str) and value, f"{name} must be an RFC3339 timestamp")
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise EvidenceError(f"{name} must be an RFC3339 timestamp") from exc


def _type7(values: list[float], probability: float) -> float:
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * probability
    lower = math.floor(position)
    fraction = position - lower
    if lower + 1 >= len(ordered):
        return ordered[-1]
    return ordered[lower] + fraction * (ordered[lower + 1] - ordered[lower])


def _numeric_summary(values: Iterable[float]) -> dict[str, Any]:
    items = list(values)
    _require(bool(items), "cannot summarize an empty measurement")
    return {
        "count": len(items),
        "min": min(items),
        "p50": _type7(items, 0.50),
        "p95": _type7(items, 0.95),
        "max": max(items),
        "mean": statistics.fmean(items),
    }


def _stable_float_sum(values: Iterable[float]) -> float:
    """Sum binary64 measurements independently of the Python ``sum`` version.

    Python 3.12 changed the algorithm used by the built-in ``sum`` for floats.
    The retained campaign was summarized with the newer algorithm, so a naive
    three-term cycle sum can differ by one ULP on Python 3.11.  ``math.fsum``
    has the same accurate-summation contract across the supported runtimes and
    lets the validator keep exact (rather than tolerance-based) evidence
    comparisons.
    """
    return math.fsum(values)


@functools.lru_cache(maxsize=1)
def _expected_dataset_fingerprints() -> dict[str, str]:
    """Independently derive the four ordered PostgreSQL MD5 fingerprints.

    The calculation mirrors the archived integer fixture formula and numeric
    display scale, not values copied from ``dataset-manifest.json``.  Matching
    all four hashes therefore checks the exact sorted-prefix/tail-churn row
    contents represented by that manifest.
    """
    base_orders = ROWS // 5
    churn_orders = (ROWS // 100) // 5
    values: dict[str, str] = {}
    for day_index, day in enumerate(DAYS):
        digest = hashlib.md5(usedforsecurity=False)
        first = True
        if day_index < 3:
            orders: Iterable[int] = range(1, base_orders + 1)
        else:
            orders = (
                *range(1, base_orders - churn_orders + 1),
                *range(base_orders + 1, base_orders + churn_orders + 1),
            )
        for order_key in orders:
            for line_number in range(1, 6):
                cents = ((order_key * 13 + line_number * 7) % 100_000) + 100
                ordinal = (order_key - 1) * 5 + line_number
                if day_index == 1 and ordinal <= ROWS // 100:
                    cents += 100
                elif day_index == 2 and ordinal <= ROWS * 5 // 100:
                    cents += 200
                elif day_index == 3 and order_key <= base_orders and ordinal <= ROWS * 10 // 100:
                    cents += 300
                row = (
                    f"1|{order_key}|{line_number}|{cents // 100}.{cents % 100:02d}"
                ).encode("ascii")
                if not first:
                    digest.update(b"\n")
                digest.update(row)
                first = False
        values[day] = digest.hexdigest()
    return values


def _validate_pack_manifest(pack: pathlib.Path) -> dict[str, Any]:
    manifest_path = pack / "pack-manifest.json"
    manifest = load_json(manifest_path)
    _require(manifest.get("schema_version") == SCHEMA, "pack manifest schema differs")
    _require(manifest.get("run_id") == RUN_ID, "pack manifest run_id differs")
    files = manifest.get("files")
    _require(isinstance(files, dict) and files, "pack manifest files must be a map")

    paths = list(pack.rglob("*"))
    _require(not any(path.is_symlink() for path in paths), "evidence pack contains a symlink")
    _require(all(path.is_dir() or path.is_file() for path in paths),
             "evidence pack contains a non-regular filesystem object")
    actual = {
        path.relative_to(pack).as_posix()
        for path in paths
        if path.is_file() and path != manifest_path
    }
    _require(set(files) == actual, "pack manifest does not enumerate the exact file set")
    for relative, descriptor in files.items():
        safe = _safe_relative(relative, f"pack file {relative!r}")
        _require(isinstance(descriptor, dict), f"pack descriptor {relative} must be an object")
        _require(set(descriptor) == {"bytes", "role", "sha256"},
                 f"pack descriptor {relative} fields differ")
        _require(isinstance(descriptor["role"], str) and descriptor["role"],
                 f"pack descriptor {relative} role is absent")
        _require(isinstance(descriptor["bytes"], int) and descriptor["bytes"] > 0,
                 f"pack descriptor {relative} byte count is invalid")
        _require_digest(descriptor["sha256"], f"pack descriptor {relative}.sha256")
        path = pack.joinpath(*safe.parts)
        _require(path.stat().st_size == descriptor["bytes"],
                 f"packed file {relative} byte count differs")
        _require(file_sha256(path) == descriptor["sha256"],
                 f"packed file {relative} SHA-256 differs")

    combined = sha256_bytes(canonical_bytes(files))
    _require(manifest.get("combined_sha256") == combined,
             "pack manifest combined SHA-256 differs")
    policy = manifest.get("binary_payload_policy")
    _require(isinstance(policy, dict), "pack binary payload policy is absent")
    _require(policy.get("hot_cold_sidecar_bytes_retained") is False,
             "pack must not claim that omitted binary bytes are retained")
    _require(policy.get("descriptors_retained") is True,
             "pack must retain binary transport descriptors")
    return manifest


def _read_tar(path: pathlib.Path) -> dict[str, bytes]:
    members: dict[str, bytes] = {}
    try:
        with tarfile.open(path, "r:") as archive:
            for member in archive.getmembers():
                _safe_relative(member.name, f"archive member {member.name!r}")
                _require(member.isfile(), f"archive member {member.name} is not regular")
                _require(member.uid == 0 and member.gid == 0 and member.mtime == 0,
                         f"archive member {member.name} metadata is not deterministic")
                _require(member.name not in members, f"archive repeats {member.name}")
                stream = archive.extractfile(member)
                _require(stream is not None, f"cannot read archive member {member.name}")
                members[member.name] = stream.read()
    except (OSError, tarfile.TarError) as exc:
        raise EvidenceError(f"read source archive {path}: {exc}") from exc
    return members


def _validate_archive(
    pack: pathlib.Path, descriptor: dict[str, Any], expected_members: dict[str, Any], name: str
) -> dict[str, bytes]:
    _require(isinstance(descriptor, dict), f"{name} archive descriptor is absent")
    relative = _safe_relative(descriptor.get("path", ""), f"{name} archive path")
    path = pack.joinpath(*relative.parts)
    _require(descriptor.get("format") == "gnu-tar", f"{name} archive format differs")
    _require(descriptor.get("deterministic_mtime_unix") == 0,
             f"{name} archive deterministic mtime differs")
    _require(descriptor.get("numeric_owner") == "0:0",
             f"{name} archive deterministic owner differs")
    _require(path.stat().st_size == descriptor.get("bytes"), f"{name} archive bytes differ")
    _require(file_sha256(path) == descriptor.get("sha256"), f"{name} archive SHA-256 differs")

    members = _read_tar(path)
    _require(set(members) == set(expected_members), f"{name} archive member set differs")
    for relative, member_descriptor in expected_members.items():
        _safe_relative(relative, f"{name} member {relative!r}")
        _require(isinstance(member_descriptor, dict), f"{name} member descriptor differs")
        body = members[relative]
        _require(len(body) == member_descriptor.get("bytes"),
                 f"{name} member {relative} byte count differs")
        _require(sha256_bytes(body) == member_descriptor.get("sha256"),
                 f"{name} member {relative} SHA-256 differs")
    return members


def _validate_source_snapshot(
    pack: pathlib.Path, results: dict[str, Any]
) -> tuple[dict[str, bytes], dict[str, bytes], dict[str, Any]]:
    source = load_json(pack / "source-manifest.json")
    _require(source.get("schema_version") == SOURCE_SCHEMA, "source manifest schema differs")
    _require(source.get("run_id") == RUN_ID, "source manifest run_id differs")
    provenance = results.get("provenance", {}).get("exact_source", {})
    run_files = provenance.get("files")
    _require(isinstance(run_files, dict) and len(run_files) == 33,
             "results must bind exactly the 33 originally recorded source files")
    recorded = source.get("run_bound_members")
    _require(isinstance(recorded, dict) and set(recorded) == set(run_files),
             "run-bound source member set differs from results")
    for relative, digest in run_files.items():
        _require_digest(digest, f"results source {relative}")
        _require(recorded[relative].get("sha256") == digest,
                 f"run-bound source {relative} differs from results")
    combined = sha256_bytes(canonical_bytes(run_files))
    _require(provenance.get("combined_sha256") == combined,
             "results source combined SHA-256 differs")
    _require(source.get("run_bound_combined_sha256") == combined,
             "source manifest combined SHA-256 differs")
    run_members = _validate_archive(
        pack, source.get("run_bound_archive", {}), recorded, "run-bound source"
    )

    supplemental = source.get("post_run_supplemental_members")
    _require(isinstance(supplemental, dict) and set(supplemental) == {
        "db/init/00-schema.sql",
        "evaluation/cmd/v4-offline/main_test.go",
        "evaluation/daily-publication/cmd/phase/main_test.go",
    }, "post-run supplemental source set differs")
    supplemental_members = _validate_archive(
        pack,
        source.get("post_run_supplemental_archive", {}),
        supplemental,
        "post-run supplemental source",
    )
    _require(source.get("post_run_supplemental_is_run_bound") is False,
             "supplemental source must not be represented as run-bound")
    limitation = source.get("provenance_limitation")
    _require(isinstance(limitation, str) and "not covered" in limitation.lower(),
             "source provenance limitation is not explicit")
    return run_members, supplemental_members, source


def _validate_raw_provenance(pack: pathlib.Path, results: dict[str, Any]) -> None:
    raw = results.get("provenance", {}).get("raw_evidence", {})
    files = raw.get("files")
    _require(isinstance(files, dict) and len(files) == 78,
             "results must enumerate exactly 78 campaign JSON files")
    actual: set[str] = {"dataset-manifest.json"}
    for directory in ("candidate-inputs", "approved-inputs", "calibration", "raw"):
        actual.update(
            path.relative_to(pack).as_posix()
            for path in (pack / directory).rglob("*.json")
            if path.is_file()
        )
    _require(set(files) == actual, "results raw provenance file set differs from packed campaign JSON")
    for relative, expected in files.items():
        _require_digest(expected, f"raw provenance {relative}")
        path = pack.joinpath(*_safe_relative(relative, relative).parts)
        _require(file_sha256(path) == expected, f"raw campaign JSON {relative} SHA-256 differs")
    _require(
        raw.get("combined_sha256") == sha256_bytes(canonical_bytes(files)),
        "results raw evidence combined SHA-256 differs",
    )


def _validate_dataset(
    pack: pathlib.Path, results: dict[str, Any], source_members: dict[str, bytes]
) -> dict[str, Any]:
    dataset = load_json(pack / "dataset-manifest.json")
    _require(dataset == results.get("dataset"), "results dataset differs from retained manifest")
    _require(dataset.get("schema_version") == DATASET_SCHEMA, "dataset schema differs")
    _require(dataset.get("generator") == "deterministic TPC-H-shaped orders/lineitem fixture",
             "dataset generator label differs")
    _require(dataset.get("postgres_version") == "16.14 (Debian 16.14-1.pgdg12+1)",
             "dataset PostgreSQL version differs")
    _require(dataset.get("rows") == {day: ROWS for day in DAYS},
             "dataset must contain exactly 345000 rows in every publication")
    expected_changes = {
        "day1": {"updated_rows": 3_450, "inserted_rows": 0, "deleted_rows": 0},
        "day2": {"updated_rows": 17_250, "inserted_rows": 0, "deleted_rows": 0},
        "day3": {"updated_rows": 34_500, "inserted_rows": 3_450, "deleted_rows": 3_450},
    }
    _require(dataset.get("changes_from_previous") == expected_changes,
             "dataset change schedule differs")
    fingerprints = dataset.get("ordered_row_fingerprint_md5")
    _require(isinstance(fingerprints, dict) and set(fingerprints) == set(DAYS),
             "dataset fingerprints are incomplete")
    _require(all(isinstance(value, str) and MD5_RE.fullmatch(value) for value in fingerprints.values()),
             "dataset fingerprints must be lowercase MD5 values")
    _require(len(set(fingerprints.values())) == 4, "dataset fingerprints must be distinct")
    _require(fingerprints == _expected_dataset_fingerprints(),
             "dataset fingerprints differ from the independently derived sorted-prefix fixture")

    generator = source_members["evaluation/daily-publication/sql/05-generate-daily-data.sh"].decode("utf-8")
    required_generator_fragments = (
        "changed_day1=$((rows / 100))",
        "changed_day2=$((rows * 5 / 100))",
        "changed_day3=$((rows * 10 / 100))",
        "churn_rows=$((rows / 100))",
        "((l_orderkey - 1) * 5 + l_linenumber) <= :changed_day1",
        "((l_orderkey - 1) * 5 + l_linenumber) <= :changed_day2",
        "((l_orderkey - 1) * 5 + l_linenumber) <= :changed_day3",
        "WHERE l_orderkey <= (:base_orders - :churn_orders)",
        "OR l_orderkey > :base_orders",
    )
    _require(all(fragment in generator for fragment in required_generator_fragments),
             "archived generator does not implement the declared sorted-prefix/tail-churn fixture")
    validation_sql = source_members["evaluation/daily-publication/sql/dataset-manifest.sql"].decode("utf-8")
    _require("'inserted_rows', 0, 'deleted_rows', 0" in validation_sql,
             "archived validation SQL changed unexpectedly")
    return dataset


def _expected_publication(day: str) -> str:
    return f"daily-lineitem-{day}-r{ROWS}"


def _publication_identity(publication: dict[str, Any]) -> tuple[Any, ...]:
    keys = (
        "publication_name", "row_count", "manifest_digest", "dictionary_digest",
        "sidecar_digest", "cold_payload_digest", "hot_index_digest",
        "artifact_bytes", "hot_artifact_bytes",
    )
    _require(set(publication) == set(keys), "publication measurement fields differ")
    for key in keys:
        if key.endswith("digest"):
            _require_digest(publication.get(key), f"publication.{key}")
    _require(publication.get("row_count") == ROWS, "publication row count differs")
    _require(isinstance(publication.get("artifact_bytes"), int) and publication["artifact_bytes"] > 0,
             "publication artifact bytes differ")
    _require(isinstance(publication.get("hot_artifact_bytes"), int) and publication["hot_artifact_bytes"] > 0,
             "publication HOT bytes differ")
    return tuple(publication[key] for key in keys)


def _command_stdout(command: dict[str, Any]) -> bytes:
    publication = command["publications"][0]
    ordered_publication = {
        key: publication[key]
        for key in (
            "publication_name", "row_count", "manifest_digest", "dictionary_digest",
            "sidecar_digest", "cold_payload_digest", "hot_index_digest",
            "artifact_bytes", "hot_artifact_bytes",
        )
    }
    ordered: dict[str, Any] = {
        "schema_version": command["schema_version"],
        "mode": command["mode"],
        "publications": [ordered_publication],
        "total_artifact_bytes": command["total_artifact_bytes"],
        "hot_artifact_bytes": command["hot_artifact_bytes"],
    }
    if "verification_receipt_sha256" in command:
        ordered["verification_receipt_sha256"] = command["verification_receipt_sha256"]
    return (json.dumps(ordered, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8")


def _expected_argv(phase: str, receipt_sha256: str | None) -> list[str]:
    base = ["/usr/local/bin/v4-offline"]
    if phase == "build":
        return base + ["build", "-input", "/input/input.json", "-output-dir", "/artifacts"]
    if phase == "strict_verify":
        return base + [
            "verify", "-input", "/input/input.json", "-artifact-dir", "/artifacts",
            "-receipt", "/receipts/verification.json",
        ]
    _require(receipt_sha256 is not None, "activation receipt digest is absent")
    return base + [
        "activate", "-input", "/input/input.json", "-artifact-dir", "/artifacts",
        "-receipt", "/receipts/verification.json", "-receipt-sha256", receipt_sha256,
    ]


def _validate_phase(
    path: pathlib.Path,
    day: str,
    sample: int,
    phase_name: str,
    receipt_sha256: str | None = None,
) -> tuple[dict[str, Any], dict[str, Any]]:
    phase = load_json(path)
    expected_fields = {
        "schema_version", "status", "phase", "day", "sample", "executable",
        "argv_sha256", "wall_ms", "peak_rss_bytes", "peak_rss_scope", "exit_code",
        "stdout_bytes", "stdout_sha256", "stderr_bytes", "stderr_sha256",
        "command_report", "measurement_boundary",
    }
    _require(set(phase) == expected_fields, f"{path} phase report fields differ")
    _require(phase.get("schema_version") == PHASE_SCHEMA, f"{path} phase schema differs")
    _require(phase.get("status") == "pass" and phase.get("exit_code") == 0,
             f"{path} did not pass")
    _require(phase.get("phase") == phase_name and phase.get("day") == day
             and phase.get("sample") == sample, f"{path} coordinates differ")
    _require(phase.get("executable") == "v4-offline", f"{path} executable differs")
    _require(phase.get("measurement_boundary") == MEASUREMENT_BOUNDARY,
             f"{path} measurement boundary differs")
    _require(phase.get("peak_rss_scope") == RSS_SCOPE, f"{path} RSS scope differs")
    _require_positive(phase.get("wall_ms"), f"{path}.wall_ms")
    _require(isinstance(phase.get("peak_rss_bytes"), int)
             and not isinstance(phase.get("peak_rss_bytes"), bool)
             and phase["peak_rss_bytes"] > 0, f"{path}.peak_rss_bytes must be a positive integer")

    argv = _expected_argv(phase_name, receipt_sha256)
    argv_sha = sha256_bytes(json.dumps(argv, separators=(",", ":")).encode("utf-8"))
    _require(phase.get("argv_sha256") == argv_sha, f"{path} argv SHA-256 differs")
    _require(phase.get("stderr_bytes") == 0 and phase.get("stderr_sha256") == EMPTY_SHA256,
             f"{path} stderr was not empty")

    command = phase.get("command_report")
    _require(isinstance(command, dict), f"{path} command report is absent")
    expected_command_fields = {
        "schema_version", "mode", "publications", "total_artifact_bytes", "hot_artifact_bytes"
    }
    if phase_name != "build":
        expected_command_fields.add("verification_receipt_sha256")
    _require(set(command) == expected_command_fields, f"{path} command report fields differ")
    _require(command.get("schema_version") == 1 and command.get("mode") == MODES[phase_name],
             f"{path} command report mode/schema differs")
    publications = command.get("publications")
    _require(isinstance(publications, list) and len(publications) == 1
             and isinstance(publications[0], dict), f"{path} must report one publication")
    publication = publications[0]
    _publication_identity(publication)
    _require(publication.get("publication_name") == _expected_publication(day),
             f"{path} publication name differs")
    _require(command.get("total_artifact_bytes") == publication["artifact_bytes"]
             and command.get("hot_artifact_bytes") == publication["hot_artifact_bytes"],
             f"{path} command aggregate bytes differ")
    if phase_name != "build":
        _require(command.get("verification_receipt_sha256") == receipt_sha256,
                 f"{path} receipt binding differs")

    stdout = _command_stdout(command)
    _require(phase.get("stdout_bytes") == len(stdout), f"{path} stdout byte count differs")
    _require(phase.get("stdout_sha256") == sha256_bytes(stdout),
             f"{path} reconstructed stdout SHA-256 differs")
    return phase, publication


def _validate_bundle(
    path: pathlib.Path, day: str, expected_measurement: dict[str, Any]
) -> dict[str, Any]:
    body = path.read_bytes()
    bundle = load_json(path)
    _require(set(bundle) == {
        "version", "publication_name", "catalog_source", "ordinal_sidecar",
        "manifest_digest", "dictionary_manifest", "row_count", "hot", "cold", "sidecar",
    }, f"{path} bundle fields differ")
    publication = _expected_publication(day)
    _require(bundle.get("version") == BUNDLE_SCHEMA, f"{path} bundle schema differs")
    _require(bundle.get("publication_name") == publication and bundle.get("row_count") == ROWS,
             f"{path} publication coordinates differ")
    _require(bundle.get("catalog_source") == "daily_reporting", f"{path} catalog source differs")
    _require(bundle.get("ordinal_sidecar") == f"taskgate_ordinal.daily_lineitem_{day}_r{ROWS}",
             f"{path} ordinal sidecar differs")

    descriptors: dict[str, dict[str, Any]] = {}
    suffixes = {"hot": ".hot.tgord", "cold": ".cold.tgord", "sidecar": ".sidecar.ndjson"}
    for role, suffix in suffixes.items():
        descriptor = bundle.get(role)
        _require(isinstance(descriptor, dict) and set(descriptor) == {"name", "sha256", "bytes"},
                 f"{path} {role} descriptor differs")
        _require(descriptor.get("name") == publication + suffix,
                 f"{path} {role} artifact name differs")
        _require_digest(descriptor.get("sha256"), f"{path}.{role}.sha256")
        _require(isinstance(descriptor.get("bytes"), int) and descriptor["bytes"] > 0,
                 f"{path} {role} byte count differs")
        descriptors[role] = descriptor

    dictionary = bundle.get("dictionary_manifest")
    _require(isinstance(dictionary, dict), f"{path} dictionary manifest is absent")
    _require(dictionary.get("version") == "taskgate-ordinal-dictionary-v1",
             f"{path} dictionary version differs")
    for key in (
        "schema_digest", "dictionary_digest", "sidecar_digest", "cold_payload_digest",
        "hot_index_digest",
    ):
        _require_digest(dictionary.get(key), f"{path}.dictionary_manifest.{key}")
    _require(bundle.get("manifest_digest") == expected_measurement["manifest_digest"],
             f"{path} manifest digest differs from phase measurement")
    for key in ("dictionary_digest", "sidecar_digest", "cold_payload_digest", "hot_index_digest"):
        _require(dictionary.get(key) == expected_measurement[key],
                 f"{path} {key} differs from phase measurement")

    segments = dictionary.get("segments")
    _require(isinstance(segments, list) and len(segments) == 9,
             f"{path} must contain eight cell segments and one row segment")
    expected_fields = {
        "daily_lineitem.dataset_partition", "daily_lineitem.l_extendedprice",
        "daily_lineitem.l_linenumber", "daily_lineitem.l_orderkey",
        "dataset_partition", "l_extendedprice", "l_linenumber", "l_orderkey",
    }
    cell_fields: set[str] = set()
    segment_ids: set[str] = set()
    row_segments = 0
    for segment in segments:
        _require(isinstance(segment, dict) and segment.get("fact_count") == ROWS,
                 f"{path} segment fact count differs")
        _require_digest(segment.get("hashes_digest"), f"{path} segment hashes_digest")
        _require_digest(segment.get("payloads_digest"), f"{path} segment payloads_digest")
        _require(segment.get("shard") == 0, f"{path} segment shard differs")
        _require(isinstance(segment.get("id"), str) and segment["id"] not in segment_ids,
                 f"{path} segment ID is absent or repeated")
        segment_ids.add(segment["id"])
        if segment.get("kind") == "base-cell":
            _require(isinstance(segment.get("field"), str), f"{path} cell field is absent")
            cell_fields.add(segment["field"])
        elif segment.get("kind") == "base-row":
            _require(segment.get("id") == "row" and "field" not in segment,
                     f"{path} row segment differs")
            row_segments += 1
        else:
            raise EvidenceError(f"{path} segment kind differs")
    _require(cell_fields == expected_fields and row_segments == 1,
             f"{path} segment field set differs")

    artifact_bytes = len(body) + sum(item["bytes"] for item in descriptors.values())
    _require(artifact_bytes == expected_measurement["artifact_bytes"],
             f"{path} total artifact bytes differ")
    _require(descriptors["hot"]["bytes"] == expected_measurement["hot_artifact_bytes"],
             f"{path} HOT artifact bytes differ")
    _require(artifact_bytes <= TOTAL_ARTIFACT_LIMIT_BYTES,
             f"{path} exceeds the 2 GiB total-artifact compiler limit")
    _require(descriptors["hot"]["bytes"] <= HOT_ARTIFACT_LIMIT_BYTES,
             f"{path} exceeds the 160 MiB HOT compiler limit")
    return {
        "sha256": sha256_bytes(body),
        "bytes": len(body),
        "transport_descriptors": descriptors,
        "base_fact_count": sum(int(item["fact_count"]) for item in segments),
        "bundle": bundle,
    }


def _receipt_body_sha256(receipt: dict[str, Any]) -> str:
    body = copy.deepcopy(receipt)
    body["receipt_body_sha256"] = ""
    encoded = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    return sha256_bytes(RECEIPT_DIGEST_DOMAIN + encoded)


def _validate_receipt(
    path: pathlib.Path,
    day: str,
    measurement: dict[str, Any],
    bundle: dict[str, Any],
) -> tuple[str, tuple[int, int], tuple[int, int]]:
    encoded = path.read_bytes()
    receipt = load_json(path)
    _require((json.dumps(receipt, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8") == encoded,
             f"{path} receipt is not canonical JSON")
    _require(receipt.get("schema_version") == RECEIPT_SCHEMA, f"{path} receipt schema differs")
    _parse_time(receipt.get("verified_at"), f"{path}.verified_at")
    _require_digest(receipt.get("receipt_body_sha256"), f"{path}.receipt_body_sha256")
    _require(receipt.get("receipt_body_sha256") == _receipt_body_sha256(receipt),
             f"{path} receipt body digest differs")
    root = receipt.get("artifact_root")
    _require(isinstance(root, dict) and root.get("device", 0) > 0 and root.get("inode", 0) > 0,
             f"{path} artifact-root identity differs")
    publications = receipt.get("publications")
    _require(isinstance(publications, list) and len(publications) == 1,
             f"{path} receipt must contain one publication")
    publication = publications[0]
    _require(isinstance(publication, dict), f"{path} receipt publication differs")
    _require(publication.get("publication_name") == _expected_publication(day),
             f"{path} receipt publication differs")
    _require(publication.get("measurement") == measurement,
             f"{path} receipt measurement differs")
    _require(publication.get("bundle_sha256") == bundle["sha256"],
             f"{path} receipt bundle SHA-256 differs")
    directory = publication.get("directory")
    _require(isinstance(directory, dict) and directory.get("device", 0) > 0
             and directory.get("inode", 0) > 0, f"{path} publication identity differs")

    expected_artifacts = {
        f"{_expected_publication(day)}.bundle.json": {
            "name": f"{_expected_publication(day)}.bundle.json",
            "bytes": bundle["bytes"],
            "sha256": bundle["sha256"],
        },
        **{
            item["name"]: item
            for item in bundle["transport_descriptors"].values()
        },
    }
    artifacts = publication.get("artifacts")
    _require(isinstance(artifacts, list) and len(artifacts) == 4,
             f"{path} receipt artifact descriptor count differs")
    actual_names: set[str] = set()
    for artifact in artifacts:
        _require(isinstance(artifact, dict), f"{path} receipt artifact differs")
        name = artifact.get("name")
        _require(isinstance(name, str) and name in expected_artifacts and name not in actual_names,
                 f"{path} receipt artifact name differs")
        actual_names.add(name)
        expected = expected_artifacts[name]
        _require(artifact.get("bytes") == expected["bytes"]
                 and artifact.get("sha256") == expected["sha256"],
                 f"{path} receipt artifact {name} descriptor differs")
        identity = artifact.get("identity")
        _require(isinstance(identity, dict) and identity.get("device", 0) > 0
                 and identity.get("inode", 0) > 0 and identity.get("size") == artifact["bytes"],
                 f"{path} receipt artifact {name} identity differs")
    _require(actual_names == set(expected_artifacts), f"{path} receipt artifact set differs")
    return (
        sha256_bytes(encoded),
        (int(root["device"]), int(root["inode"])),
        (int(directory["device"]), int(directory["inode"])),
    )


def _validate_candidate_inputs(pack: pathlib.Path) -> dict[str, dict[str, Any]]:
    manifest = load_json(pack / "candidate-inputs/manifest.json")
    _require(manifest.get("schema_version") == INPUT_SET_SCHEMA, "candidate input-set schema differs")
    _require(manifest.get("rows") == ROWS, "candidate input-set rows differ")
    inputs = manifest.get("inputs")
    _require(isinstance(inputs, dict) and set(inputs) == set(DAYS), "candidate input-set days differ")
    result: dict[str, dict[str, Any]] = {}
    canonical_fields = {
        "dataset_partition", "daily_lineitem.dataset_partition", "l_orderkey",
        "daily_lineitem.l_orderkey", "l_linenumber", "daily_lineitem.l_linenumber",
        "l_extendedprice", "daily_lineitem.l_extendedprice",
    }
    for day in DAYS:
        descriptor = inputs[day]
        _require(isinstance(descriptor, dict), f"candidate {day} descriptor differs")
        _require(descriptor.get("path") == f"{day}.json", f"candidate {day} path differs")
        path = pack / "candidate-inputs" / f"{day}.json"
        _require(file_sha256(path) == descriptor.get("sha256"), f"candidate {day} SHA-256 differs")
        value = load_json(path)
        _require(value.get("version") == INPUT_SCHEMA, f"candidate {day} schema differs")
        _require("expected_digests" not in value, f"candidate {day} is already approved")
        _require(value.get("publication_name") == _expected_publication(day),
                 f"candidate {day} publication differs")
        _require(value.get("source_relation") == f"reporting.daily_lineitem_{day}",
                 f"candidate {day} source relation differs")
        _require(value.get("ordinal_sidecar") == f"taskgate_ordinal.daily_lineitem_{day}_r{ROWS}",
                 f"candidate {day} ordinal sidecar differs")
        snapshot = value.get("snapshot")
        _require(isinstance(snapshot, dict) and snapshot.get("rows") == [],
                 f"candidate {day} must stream rows from PostgreSQL")
        _require(snapshot.get("snapshot") == f"rq5-daily-lineitem-{day}-rows-{ROWS}",
                 f"candidate {day} snapshot differs")
        fields = snapshot.get("fields")
        _require(isinstance(fields, list) and len(fields) == 8,
                 f"candidate {day} field count differs")
        _require(all(isinstance(field, dict) for field in fields),
                 f"candidate {day} field descriptor differs")
        _require({field.get("canonical_field_id") for field in fields} == canonical_fields,
                 f"candidate {day} canonical field set differs")
        result[day] = value
    return result


def _bundle_path(pack: pathlib.Path, day: str, sample: int | None) -> pathlib.Path:
    publication = _expected_publication(day)
    if sample is None:
        root = pack / "calibration" / day
    else:
        root = pack / "raw" / day / f"sample-{sample}"
    return root / "artifacts" / publication / f"{publication}.bundle.json"


def require_exact_bundle_determinism(items: list[dict[str, Any]], day: str) -> None:
    """Require byte-identical manifests and exact transport descriptors.

    This small public helper is also used by adversarial tests to ensure that
    semantic JSON equality cannot substitute for exact manifest bytes.
    """
    _require(len(items) == RUNS + 1, f"{day} must have calibration plus three bundles")
    _require(len({item["sha256"] for item in items}) == 1,
             f"{day} bundle manifests are not byte-identical")
    identities = {
        canonical_bytes(item["transport_descriptors"])
        for item in items
    }
    _require(len(identities) == 1, f"{day} transport descriptors differ across builds")


def _validate_omissions(pack: pathlib.Path, bundles: list[tuple[str, dict[str, Any]]]) -> dict[str, Any]:
    omissions = load_json(pack / "transport-omissions.json")
    _require(omissions.get("schema_version") == OMISSION_SCHEMA, "transport omission schema differs")
    _require(omissions.get("run_id") == RUN_ID, "transport omission run_id differs")
    expected: list[dict[str, Any]] = []
    for bundle_relative, bundle in bundles:
        parent = pathlib.PurePosixPath(bundle_relative).parent
        for role in ("hot", "cold", "sidecar"):
            descriptor = bundle["transport_descriptors"][role]
            artifact_path = (parent / descriptor["name"]).as_posix()
            expected.append({
                "artifact_path": artifact_path,
                "bundle_manifest_path": bundle_relative,
                "bytes": descriptor["bytes"],
                "role": role,
                "sha256": descriptor["sha256"],
            })
            _require(not (pack / artifact_path).exists(),
                     f"omitted binary artifact is unexpectedly packed: {artifact_path}")
    expected.sort(key=lambda item: (item["bundle_manifest_path"], item["role"]))
    _require(omissions.get("artifacts") == expected, "transport omission inventory differs")
    _require(omissions.get("artifact_count") == len(expected) == 48,
             "transport omission count differs")
    _require(omissions.get("logical_bytes_omitted") == sum(item["bytes"] for item in expected),
             "transport omitted byte total differs")
    policy = omissions.get("audit_boundary")
    _require(isinstance(policy, str) and "cannot" in policy.lower()
             and "re-hash" in policy.lower(), "binary audit boundary is not explicit")
    return omissions


def _validate_environment(
    pack: pathlib.Path,
    results: dict[str, Any],
    source: dict[str, Any],
    maximum_builder_rss: int,
) -> dict[str, Any]:
    environment = load_json(pack / "environment.json")
    _require(environment.get("schema_version") == ENVIRONMENT_SCHEMA,
             "environment schema differs")
    _require(environment.get("run_id") == RUN_ID, "environment run_id differs")
    bindings = environment.get("bindings")
    _require(isinstance(bindings, dict), "environment bindings are absent")
    _require(bindings.get("results_sha256") == file_sha256(pack / "results.json"),
             "environment results binding differs")
    _require(bindings.get("run_bound_source_combined_sha256") == source["run_bound_combined_sha256"],
             "environment source binding differs")
    generated = _parse_time(results.get("generated_at"), "results.generated_at")
    captured = _parse_time(environment.get("captured_at"), "environment.captured_at")
    _require(captured >= generated, "environment capture predates completed results")
    _require(environment.get("capture_timing") == "post-run; immediately after campaign completion",
             "environment capture timing is not explicit")

    runtime = environment.get("container_runtime")
    _require(isinstance(runtime, dict) and runtime.get("cgroup_version") == "2"
             and runtime.get("cgroup_driver") == "cgroupfs",
             "container runtime cgroup provenance differs")
    images = environment.get("images")
    _require(isinstance(images, dict), "environment image provenance is absent")
    for name in ("phase", "postgres", "golang_build_base", "debian_runtime_base"):
        image = images.get(name)
        _require(isinstance(image, dict), f"environment image {name} is absent")
        _require(isinstance(image.get("image_id"), str)
                 and re.fullmatch(r"sha256:[0-9a-f]{64}", image["image_id"]),
                 f"environment image {name} ID differs")
    _require(images["phase"].get("image_id") ==
             "sha256:f8356b61119299ee44a158948aa48bb118f6c5c4856e30c702e00c31c7a5e03e",
             "phase image content ID differs")
    _require(images["postgres"].get("repo_digest") ==
             "postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55",
             "PostgreSQL image digest differs")

    resource = environment.get("resource_boundary")
    _require(isinstance(resource, dict), "resource boundary is absent")
    _require(resource.get("phase_container_memory_limit_bytes") == PHASE_MEMORY_LIMIT_BYTES,
             "phase container memory limit differs")
    _require(resource.get("builder_peak_rss_max_bytes") == maximum_builder_rss,
             "environment maximum builder VmHWM differs")
    _require(resource.get("rss_is_cgroup_or_system_peak") is False,
             "environment must not claim VmHWM is a cgroup/system peak")
    _require(resource.get("prior_4_gib_builder_envelope_satisfied") is False
             and maximum_builder_rss > PRIOR_BUILDER_ENVELOPE_BYTES,
             "prior 4 GiB builder envelope disclosure differs")

    cache = environment.get("cache_policy")
    _require(cache == {
        "calibration_precedes_measured_builds_for_each_day": True,
        "filesystem_page_cache_dropped": False,
        "postgres_buffers_reset_between_phases": False,
        "strict_verify_immediately_follows_each_build": True,
        "interpretation": "sequential warm operational path; not a cold-cache measurement",
    }, "cache policy disclosure differs")
    measurement = environment.get("measurement_boundary")
    _require(isinstance(measurement, dict)
             and measurement.get("phase_report_text") == MEASUREMENT_BOUNDARY
             and measurement.get("rss_scope") == RSS_SCOPE
             and measurement.get("cycle_definition") ==
             "sum of three non-contiguous child-process wall times: build + strict_verify + activation",
             "environment measurement boundary differs")

    compose = source.get("run_bound_members", {}).get("evaluation/daily-publication/compose.yaml", {})
    _require(resource.get("compose_sha256") == compose.get("sha256"),
             "resource boundary compose binding differs")
    compose_body = _read_tar(pack / source["run_bound_archive"]["path"])[
        "evaluation/daily-publication/compose.yaml"
    ].decode("utf-8")
    _require("mem_limit: 6g" in compose_body
             and resource.get("compose_declaration") == "mem_limit: 6g",
             "archived Compose memory-limit declaration differs")
    return environment


def _validate_result_contract(results: dict[str, Any], original_offline: dict[str, Any], maximum_cycle: float) -> None:
    _require(results.get("schema_version") == RESULT_SCHEMA, "results schema differs")
    _require(results.get("status") == "incomplete" and results.get("acceptance") == "incomplete",
             "offline-only RQ5 results must remain incomplete")
    configuration = results.get("configuration")
    _require(isinstance(configuration, dict), "results configuration is absent")
    _require(configuration.get("rows_per_publication") == ROWS
             and configuration.get("runs_per_publication") == RUNS
             and configuration.get("daily_cycle_gate_ms") == CYCLE_GATE_MS,
             "results scale coordinates differ")
    _require(results.get("offline") == original_offline,
             "results offline projection differs from canonical phase recomputation")
    _require(results.get("online") == {
        "status": "not_measured", "transitions": [], "latency_ms": None,
    }, "results must not assert absent online measurements")
    _require(isinstance(results.get("claim_boundary"), str)
             and "does not establish" in results["claim_boundary"],
             "results claim boundary differs")

    expected_offline_gates = [
        {
            "id": "four_version_dataset",
            "requirement": "Day0 plus exact 1%, 5%, and 10% update publications; Day3 also has inserts/deletes",
            "status": "pass",
            "evidence": {"rows_per_publication": ROWS},
        },
        {"id": "offline_execution_integrity", "requirement": "all phase evidence is valid", "status": "pass"},
        {
            "id": "three_builds_per_publication",
            "requirement": "each of four publications has at least three independent builds",
            "status": "pass",
            "evidence": {"required": RUNS, "complete_samples": 12},
        },
        {
            "id": "artifact_determinism",
            "requirement": "all builds for one day have identical semantic digests and artifact sizes",
            "status": "pass",
        },
        {
            "id": "receipt_bound_activation",
            "requirement": "every activation uses the receipt produced by strict verification of the same artifacts",
            "status": "pass",
        },
        {
            "id": "daily_cycle_under_five_minutes",
            "requirement": "build + strict verification + activation <= 300000 ms for every sample",
            "status": "pass",
            "evidence": {"maximum_cycle_ms": maximum_cycle, "threshold_ms": CYCLE_GATE_MS},
        },
    ]
    gates = results.get("gates")
    _require(isinstance(gates, list) and gates[:6] == expected_offline_gates,
             "results offline gates differ from canonical recomputation")
    _require(len(gates) == 12 and all(item.get("status") == "unmeasured" for item in gates[6:]),
             "results online gates must remain unmeasured")


def recompute_canonical(pack_path: pathlib.Path | str = DEFAULT_PACK) -> dict[str, Any]:
    """Recompute the authoritative offline summary from retained primary JSON.

    This function deliberately does not trust ``canonical-offline.json`` or the
    summary fields in ``results.json``.  It is the importable API intended for
    paper/table generation.
    """
    pack = pathlib.Path(pack_path).resolve()
    results = load_json(pack / "results.json")
    _validate_raw_provenance(pack, results)
    source_members, _supplemental, source = _validate_source_snapshot(pack, results)
    dataset = _validate_dataset(pack, results, source_members)
    candidates = _validate_candidate_inputs(pack)

    config_body = source_members["evaluation/daily-publication/config.json"]
    _require(sha256_bytes(config_body) == results["configuration"]["sha256"],
             "archived config SHA-256 differs from results")
    config = json.loads(
        config_body,
        object_pairs_hook=_reject_duplicate_pairs,
        parse_constant=_reject_nonfinite,
    )
    _require(config.get("schema_version") == CONFIG_SCHEMA
             and config.get("runs_per_publication") == RUNS
             and config.get("daily_cycle_gate_ms") == CYCLE_GATE_MS,
             "archived campaign config differs")
    _require(config.get("dataset", {}).get("publication_scale_rows") == ROWS,
             "archived config scale row count differs")

    day_outputs: list[dict[str, Any]] = []
    harness_days: list[dict[str, Any]] = []
    all_bundles: list[tuple[str, dict[str, Any]]] = []
    all_cycles: list[float] = []
    all_builds: list[float] = []
    all_verifies: list[float] = []
    all_activations: list[float] = []
    all_builder_rss: list[int] = []
    receipt_root_identities: set[tuple[int, int]] = set()
    receipt_publication_identities: set[tuple[int, int]] = set()

    for day in DAYS:
        calibration_path = pack / "calibration" / day / "build.json"
        calibration_phase, calibration_measurement = _validate_phase(
            calibration_path, day, 0, "build"
        )
        calibration_bundle_path = _bundle_path(pack, day, None)
        calibration_bundle = _validate_bundle(
            calibration_bundle_path, day, calibration_measurement
        )
        all_bundles.append((calibration_bundle_path.relative_to(pack).as_posix(), calibration_bundle))

        approved_path = pack / "approved-inputs" / f"{day}.json"
        approved = load_json(approved_path)
        approved_without_digests = copy.deepcopy(approved)
        expected_digests = approved_without_digests.pop("expected_digests", None)
        _require(approved_without_digests == candidates[day],
                 f"approved input {day} differs from candidate beyond expected digests")
        expected_approval = {
            key: calibration_measurement[key]
            for key in (
                "sidecar_digest", "dictionary_digest", "manifest_digest",
                "cold_payload_digest", "hot_index_digest",
            )
        }
        _require(expected_digests == expected_approval,
                 f"approved input {day} does not bind calibration digests")

        bundle_runs = [calibration_bundle]
        harness_samples: list[dict[str, Any]] = []
        canonical_samples: list[dict[str, Any]] = []
        semantic_identities: list[tuple[Any, ...]] = []
        for sample in range(1, RUNS + 1):
            sample_root = pack / "raw" / day / f"sample-{sample}"
            build_phase, build_measurement = _validate_phase(
                sample_root / "build.json", day, sample, "build"
            )
            _require(_publication_identity(build_measurement) == _publication_identity(calibration_measurement),
                     f"{day} sample {sample} differs from calibrated publication")
            bundle_path = _bundle_path(pack, day, sample)
            bundle = _validate_bundle(bundle_path, day, build_measurement)
            bundle_runs.append(bundle)
            all_bundles.append((bundle_path.relative_to(pack).as_posix(), bundle))

            receipt_path = sample_root / "receipt" / "verification.json"
            receipt_sha, root_identity, publication_identity = _validate_receipt(
                receipt_path, day, build_measurement, bundle
            )
            _require(root_identity not in receipt_root_identities,
                     f"{day} sample {sample} reuses an artifact-root identity")
            _require(publication_identity not in receipt_publication_identities,
                     f"{day} sample {sample} reuses a publication-directory identity")
            receipt_root_identities.add(root_identity)
            receipt_publication_identities.add(publication_identity)
            verify_phase, verify_measurement = _validate_phase(
                sample_root / "strict_verify.json", day, sample, "strict_verify", receipt_sha
            )
            activation_phase, activation_measurement = _validate_phase(
                sample_root / "activation.json", day, sample, "activation", receipt_sha
            )
            identity = _publication_identity(build_measurement)
            _require(_publication_identity(verify_measurement) == identity
                     and _publication_identity(activation_measurement) == identity,
                     f"{day} sample {sample} publication differs across phases")
            semantic_identities.append(identity)

            walls = {
                "build": float(build_phase["wall_ms"]),
                "strict_verify": float(verify_phase["wall_ms"]),
                "activation": float(activation_phase["wall_ms"]),
            }
            peaks = {
                "build": int(build_phase["peak_rss_bytes"]),
                "strict_verify": int(verify_phase["peak_rss_bytes"]),
                "activation": int(activation_phase["peak_rss_bytes"]),
            }
            cycle = _stable_float_sum(walls[phase] for phase in PHASES)
            all_cycles.append(cycle)
            all_builds.append(walls["build"])
            all_verifies.append(walls["strict_verify"])
            all_activations.append(walls["activation"])
            all_builder_rss.append(peaks["build"])
            harness_sample = {
                "sample": sample,
                "wall_ms": walls,
                "cycle_ms": cycle,
                "peak_rss_bytes": peaks,
                "row_count": build_measurement["row_count"],
                "artifact_bytes": build_measurement["artifact_bytes"],
                "hot_artifact_bytes": build_measurement["hot_artifact_bytes"],
                "manifest_digest": build_measurement["manifest_digest"],
                "dictionary_digest": build_measurement["dictionary_digest"],
                "verification_receipt_sha256": receipt_sha,
            }
            harness_samples.append(harness_sample)
            canonical_samples.append({
                **harness_sample,
                "bundle_manifest_sha256": bundle["sha256"],
                "transport_descriptors": bundle["transport_descriptors"],
            })

        _require(len(set(semantic_identities)) == 1, f"{day} semantic publications differ")
        require_exact_bundle_determinism(bundle_runs, day)
        summary = {
            "build_ms": _numeric_summary([item["wall_ms"]["build"] for item in harness_samples]),
            "strict_verify_ms": _numeric_summary([item["wall_ms"]["strict_verify"] for item in harness_samples]),
            "activation_ms": _numeric_summary([item["wall_ms"]["activation"] for item in harness_samples]),
            "cycle_ms": _numeric_summary([item["cycle_ms"] for item in harness_samples]),
            "builder_peak_rss_bytes": _numeric_summary(
                [float(item["peak_rss_bytes"]["build"]) for item in harness_samples]
            ),
            "artifact_bytes": harness_samples[0]["artifact_bytes"],
            "hot_artifact_bytes": harness_samples[0]["hot_artifact_bytes"],
        }
        harness_days.append({
            "id": day,
            "samples": harness_samples,
            "artifact_deterministic": True,
            "summary": summary,
        })
        day_outputs.append({
            "id": day,
            "calibration": {
                "wall_ms": float(calibration_phase["wall_ms"]),
                "builder_peak_rss_bytes": int(calibration_phase["peak_rss_bytes"]),
                "bundle_manifest_sha256": calibration_bundle["sha256"],
            },
            "samples": canonical_samples,
            "summary": summary,
            "determinism": {
                "semantic_publication_identity": True,
                "bundle_manifest_exact_bytes": True,
                "transport_descriptors": True,
                "includes_calibration_and_three_measured_builds": True,
            },
            "bundle_manifest_sha256": calibration_bundle["sha256"],
            "transport_descriptors": calibration_bundle["transport_descriptors"],
            "facts_per_publication": calibration_bundle["base_fact_count"],
        })

    original_offline = {"status": "complete", "days": harness_days}
    maximum_cycle = max(all_cycles)
    _validate_result_contract(results, original_offline, maximum_cycle)
    omissions = _validate_omissions(pack, all_bundles)
    maximum_builder_rss = max(all_builder_rss)
    environment = _validate_environment(pack, results, source, maximum_builder_rss)

    gates = [
        {"id": "exact_345000_row_sorted_prefix_fixture", "status": "pass"},
        {"id": "three_measured_builds_per_day", "status": "pass"},
        {"id": "phase_command_and_stdout_integrity", "status": "pass"},
        {"id": "calibration_approval_binding", "status": "pass"},
        {"id": "receipt_bound_activation", "status": "pass"},
        {"id": "distinct_measured_artifact_roots", "status": "pass"},
        {"id": "bundle_manifest_exact_byte_determinism", "status": "pass"},
        {"id": "transport_descriptor_determinism", "status": "pass"},
        {"id": "two_gib_total_and_160_mib_hot_artifact_limits", "status": "pass"},
        {
            "id": "daily_cycle_under_five_minutes",
            "status": "pass" if maximum_cycle <= CYCLE_GATE_MS else "fail",
            "evidence": {"maximum_cycle_ms": maximum_cycle, "threshold_ms": CYCLE_GATE_MS},
        },
    ]
    return {
        "schema_version": CANONICAL_SCHEMA,
        "run_id": RUN_ID,
        "offline_status": "complete",
        "rq5_overall_status": "incomplete_without_online_transition_evidence",
        "bindings": {
            "results_sha256": file_sha256(pack / "results.json"),
            "raw_evidence_combined_sha256": results["provenance"]["raw_evidence"]["combined_sha256"],
            "run_bound_source_combined_sha256": source["run_bound_combined_sha256"],
            "environment_sha256": file_sha256(pack / "environment.json"),
            "transport_omissions_sha256": file_sha256(pack / "transport-omissions.json"),
        },
        "workload": {
            "rows_per_publication": ROWS,
            "publications": 4,
            "measured_builds_per_publication": RUNS,
            "fixture": "deterministic TPC-H-shaped; not an audited TPC-H benchmark",
            "change_selection": "ascending ((l_orderkey - 1) * 5 + l_linenumber) prefix; day3 deletes the old tail and inserts the next key range",
            "changes_from_previous": dataset["changes_from_previous"],
            "facts_per_row": 9,
            "facts_per_publication": ROWS * 9,
            "publication_shape": "one daily_lineitem publication with eight base-cell segments and one base-row segment",
        },
        "metrics": {
            "maximum_cycle_ms": maximum_cycle,
            "maximum_build_ms": max(all_builds),
            "maximum_strict_verify_ms": max(all_verifies),
            "maximum_activation_ms": max(all_activations),
            "maximum_builder_peak_rss_bytes": maximum_builder_rss,
            "phase_container_memory_limit_bytes": PHASE_MEMORY_LIMIT_BYTES,
            "prior_4_gib_builder_envelope_satisfied": False,
            "artifact_bytes_by_day": {
                value["id"]: value["summary"]["artifact_bytes"] for value in day_outputs
            },
            "hot_artifact_bytes_by_day": {
                value["id"]: value["summary"]["hot_artifact_bytes"] for value in day_outputs
            },
        },
        "measurement_boundary": environment["measurement_boundary"],
        "cache_policy": environment["cache_policy"],
        "days": day_outputs,
        "gates": gates,
        "binary_payloads": {
            "retained": False,
            "omitted_artifact_count": omissions["artifact_count"],
            "logical_bytes_omitted": omissions["logical_bytes_omitted"],
            "fresh_clone_audit_boundary": omissions["audit_boundary"],
        },
        "limitations": [
            "Online task-binding, ledger-isolation, cache, delegation, and transition-latency evidence is absent; overall RQ5 is incomplete.",
            "HOT/COLD/sidecar bytes are omitted, so a fresh clone validates retained descriptors and receipt/bundle chains but cannot independently re-hash payload bytes.",
            "VmHWM is the direct v4-offline root process peak, not cgroup, PostgreSQL, page-cache, or full-system peak memory.",
            "Cycle time sums three child-process intervals and excludes container startup, orchestration, and calibration.",
            "OS/PostgreSQL caches were not reset; this is a sequential warm operational-path measurement, not cold-cache performance.",
            "The dataset manifest measures l_extendedprice updates; day1/day2 zero insert/delete values are hard-coded by the archived validation SQL rather than independently counted.",
            "db/init/00-schema.sql and two build-time test files were omitted from the original 33-file run-bound source map; their separately retained bytes are post-run supplemental only.",
        ],
    }


def validate_pack(pack_path: pathlib.Path | str = DEFAULT_PACK) -> dict[str, Any]:
    """Validate the sealed pack and return its recomputed canonical summary."""
    pack = pathlib.Path(pack_path).resolve()
    _require(pack.is_dir(), f"evidence pack does not exist: {pack}")
    manifest = _validate_pack_manifest(pack)
    canonical = recompute_canonical(pack)
    retained = load_json(pack / "canonical-offline.json")
    _require(retained == canonical, "retained canonical-offline.json differs from recomputation")
    _require(manifest.get("canonical_offline_sha256") == file_sha256(pack / "canonical-offline.json"),
             "pack manifest canonical summary binding differs")
    _require(manifest.get("results_sha256") == file_sha256(pack / "results.json"),
             "pack manifest results binding differs")
    return canonical


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pack", type=pathlib.Path, default=DEFAULT_PACK,
                        help="evidence pack directory")
    parser.add_argument("--json", action="store_true",
                        help="print the full recomputed canonical summary")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        canonical = validate_pack(args.pack)
    except EvidenceError as exc:
        print(f"RQ5 evidence validation failed: {exc}", file=sys.stderr)
        return 1
    if args.json:
        print(json.dumps(canonical, ensure_ascii=False, indent=2, sort_keys=True))
    else:
        metrics = canonical["metrics"]
        print(
            "RQ5 offline evidence valid: "
            f"rows={canonical['workload']['rows_per_publication']} "
            f"builds={canonical['workload']['measured_builds_per_publication']}x4 "
            f"max_cycle_ms={metrics['maximum_cycle_ms']:.6f} "
            f"max_builder_vmhwm_bytes={metrics['maximum_builder_peak_rss_bytes']} "
            f"overall={canonical['rq5_overall_status']}"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
