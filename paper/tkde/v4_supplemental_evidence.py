#!/usr/bin/env python3
"""Fail-closed validator for TaskGate V4 supplemental evidence.

This module is intentionally independent from ``generate_evidence.py`` and
the original V4 acceptance validator.  It validates the three supplemental
axes (bitmap distribution, shared-root concurrency, and the independent
million-Fact oracle), their exact artifact manifest, and a generalized source
scope covering both the supplemental code and the production code it calls.

The public entry point accepts an alternate evidence directory so a campaign
can be validated before its artifacts are copied into the source-controlled
location.  It never creates, rewrites, or repairs evidence.
"""

from __future__ import annotations

import hashlib
import io
import json
import math
import os
import re
import stat
import struct
import tarfile
from datetime import datetime
from pathlib import Path, PurePosixPath
from typing import Any, Iterable


EVIDENCE_REL = Path("evaluation/v4-supplemental/evidence")
V4_RESULTS_REL = Path("evaluation/v4-acceptance/evidence/results.json")
CONCURRENCY_SOURCE_REL = Path("evaluation/cmd/v4-concurrency/source.go")
ORACLE_SOURCE_REL = Path("evaluation/v4oracle/source.go")
VALIDATOR_REL = Path("paper/tkde/v4_supplemental_evidence.py")
HISTORICAL_SOURCE_REL = Path(
    "evaluation/v4-supplemental/history/historical-source-fede479.json")
HISTORICAL_ARCHIVE_REL = Path(
    "evaluation/v4-supplemental/history/historical-source-fede479.tar.gz")

LOCAL_ARTIFACTS = {
    "README.md",
    "environment.json",
    "distribution.json",
    "concurrency-config.json",
    "concurrency.json",
    "million-oracle.json",
}
EXPECTED_LOCAL_FILES = LOCAL_ARTIFACTS | {"manifest.json"}
EXPECTED_ARTIFACTS = {
    *(f"{EVIDENCE_REL.as_posix()}/{name}" for name in LOCAL_ARTIFACTS),
    V4_RESULTS_REL.as_posix(),
}
MAXIMUMS = {
    "README.md": 1 << 20,
    "environment.json": 1 << 20,
    "distribution.json": 8 << 20,
    "concurrency-config.json": 4 << 20,
    "concurrency.json": 16 << 20,
    "million-oracle.json": 8 << 20,
    "manifest.json": 1 << 20,
    V4_RESULTS_REL.as_posix(): 16 << 20,
}

MANIFEST_SOURCE_ALGORITHM = "sha256(path\\0bytes\\0) over sorted generalized supplemental source scope v1"
HISTORICAL_ARCHIVE_GENERATION = (
    "git -c tar.umask=0022 archive --format=tar <commit> -- "
    "<sorted generalized source paths> | gzip -n -9"
)
HISTORICAL_SOURCE_SELECTION = (
    "generalized supplemental source scope reconstructed from archived "
    "concurrency/oracle declarations and fixed validator inputs"
)
EXPECTED_HISTORICAL_CAMPAIGN = "taskgate-v4-supplemental-fede479"
EXPECTED_HISTORICAL_COMMIT = "fede4798add8bb7bbf5793466efc9cf857c4bb8a"
EXPECTED_HISTORICAL_TREE = "24a3f56ed5d5f53b6fe6595c70563ae4bfff5701"
EXPECTED_HISTORICAL_ARCHIVE_SHA256 = (
    "6f09478f6b7a8f7e75790574425d84f14511bde725af8273d0ba1f207932c329")
EXPECTED_HISTORICAL_SCOPE_SHA256 = (
    "27ac9c29d845b62616f968738559387bd9adbeb9b91f0017684156878a26f6e3")
EXPECTED_HISTORICAL_PATHS_SHA256 = (
    "773e4181046db2fc7f71ca4edaa0e5120534a863ee5044e2216336e528f30808")
EXPECTED_HISTORICAL_CONCURRENCY_SHA256 = (
    "127538d36adc3de86000912a264c22f1383547fa0e4019222b21db619c3163a7")
EXPECTED_HISTORICAL_ORACLE_REPOSITORY_SHA256 = (
    "7940e52f97d3c181f2faf3110dd66b152bab2bb57b075dde2eab533d71a0b190")
EXPECTED_HISTORICAL_ORACLE_PACKAGE_SHA256 = (
    "9d08afa983fd48ae6a98187e79666fdbf76b0be7cf1791591b355b1ce9c488ed")
EXPECTED_HISTORICAL_SCOPE_FILES = 138
EXPECTED_HISTORICAL_CONCURRENCY_FILES = 113
EXPECTED_HISTORICAL_ORACLE_REPOSITORY_FILES = 52
EXPECTED_HISTORICAL_ARCHIVE_MEMBERS = 157
EXPECTED_HISTORICAL_MTIME = 1_785_410_678
HEX64 = re.compile(r"^[0-9a-f]{64}$")
MAX_POINT = (12, 1_035_000, 1)
TOTAL_MAX_POINT = sum(MAX_POINT)
EXPECTED_DISTRIBUTION_MATRIX_SHA256 = "d7af8eb994b415b0d4b75d208c829909eb277adeeb3475ad1a3d9917537fdd41"
EXPECTED_CONCURRENCY_CATALOG_SHA256 = "5b213f491ab84f3c10425c07f519b8f6a5a620719cde1aed87b2ae16a8c30c26"
EXPECTED_DISTRIBUTION_EFFECTS = {
    "dense": {
        "container_count": 16,
        "portable_bitmap_bytes": 240,
        "digest": "d966d41520f5f0cc0892cf046374b7905bd62773473725df0b737efe6a56765f",
        "minimum_ordinal": 0,
        "maximum_ordinal": 1_034_999,
    },
    "clustered": {
        "container_count": 142,
        "portable_bitmap_bytes": 2_130,
        "digest": "2c34f1caeafe10fce2780bd1b5753f6eed9b1cbd6d1857f6afcc5b00b33ae9c5",
        "minimum_ordinal": 0,
        "maximum_ordinal": 4_228_386_724,
    },
    "random_sparse": {
        "container_count": 65_536,
        "portable_bitmap_bytes": 3_118_576,
        "digest": "24c56cb75f0d1862ffe1b18c188818997ccf8caede4a5f0bd582c61cf896110a",
        "minimum_ordinal": 4_227,
        "maximum_ordinal": 4_294_963_250,
    },
}
EXPECTED_ORACLE_GATES = {
    "independent_boundary",
    "million_fact_identity",
    "derived_witness_identity",
    "outcome_and_observation_identity",
    "bounded_external_merge",
}


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError("V4 supplemental evidence: " + message)


def _object_no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def _reject_constant(value: str) -> None:
    raise ValueError(f"non-finite JSON number {value}")


def _decode_json(raw: bytes, label: str) -> dict[str, Any]:
    try:
        value = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_object_no_duplicates,
            parse_constant=_reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        raise ValueError(f"V4 supplemental evidence: invalid {label}: {error}") from error
    _require(isinstance(value, dict), f"{label} must be one JSON object")
    return value


def _read_regular(path: Path, maximum: int) -> bytes:
    try:
        before = os.lstat(path)
    except OSError as error:
        raise ValueError(f"V4 supplemental evidence: cannot stat {path}: {error}") from error
    _require(stat.S_ISREG(before.st_mode) and not stat.S_ISLNK(before.st_mode),
             f"{path} is not a regular non-symlink file")
    _require(0 < before.st_size <= maximum, f"{path} has an invalid size")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "rb") as handle:
            opened = os.fstat(handle.fileno())
            raw = handle.read(maximum + 1)
            after = os.fstat(handle.fileno())
        after_path = os.lstat(path)
    except OSError as error:
        raise ValueError(f"V4 supplemental evidence: cannot read {path}: {error}") from error
    identity = lambda one: (one.st_dev, one.st_ino, one.st_size, one.st_mtime_ns)
    _require(identity(before) == identity(opened) == identity(after) == identity(after_path),
             f"{path} changed while read")
    _require(len(raw) == before.st_size and len(raw) <= maximum, f"{path} changed size while read")
    return raw


def _sha(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _digest(value: Any, label: str) -> str:
    _require(isinstance(value, str) and HEX64.fullmatch(value) is not None,
             f"{label} is not a lowercase SHA-256")
    return value


def _integer(value: Any, label: str, *, minimum: int | None = None,
             maximum: int | None = None) -> int:
    _require(isinstance(value, int) and not isinstance(value, bool), f"{label} is not an integer")
    if minimum is not None:
        _require(value >= minimum, f"{label} is below {minimum}")
    if maximum is not None:
        _require(value <= maximum, f"{label} exceeds {maximum}")
    return value


def _number(value: Any, label: str, *, minimum: float | None = None,
            maximum: float | None = None) -> float:
    _require(isinstance(value, (int, float)) and not isinstance(value, bool) and
             math.isfinite(float(value)), f"{label} is not finite numeric evidence")
    result = float(value)
    if minimum is not None:
        _require(result >= minimum, f"{label} is below {minimum}")
    if maximum is not None:
        _require(result <= maximum, f"{label} exceeds {maximum}")
    return result


def _fields(value: Any, required: set[str], label: str,
            optional: set[str] | None = None) -> dict[str, Any]:
    _require(isinstance(value, dict), f"{label} is not an object")
    optional = optional or set()
    actual = set(value)
    _require(required <= actual and actual <= required | optional,
             f"{label} fields differ: missing={sorted(required - actual)} unknown={sorted(actual - required - optional)}")
    return value


def _list(value: Any, label: str, *, length: int | None = None) -> list[Any]:
    _require(isinstance(value, list), f"{label} is not an array")
    if length is not None:
        _require(len(value) == length, f"{label} has {len(value)} items, want {length}")
    return value


_ISO_SUBMICROSECOND = re.compile(r"(?<=:\d\d)\.(\d+)")


def _normalise_iso_fraction(text: str) -> str:
    """Truncate sub-microsecond digits the way CPython 3.11+ fromisoformat does.

    Evidence timestamps are recorded with nanosecond precision. Python 3.11 and
    later accept any number of fractional digits and truncate to microseconds;
    Python 3.10 accepts only 3 or 6 digits and rejects everything else.
    Rewriting the fraction to exactly six digits -- truncating what is longer
    and zero-padding what is shorter -- reproduces 3.11 semantics, so the host
    interpreter and the paper container validate identically.
    """
    return _ISO_SUBMICROSECOND.sub(
        lambda match: "." + match.group(1)[:6].ljust(6, "0"), text, count=1)


def _timestamp(value: Any, label: str) -> datetime:
    _require(isinstance(value, str) and value, f"{label} is not a timestamp")
    try:
        parsed = datetime.fromisoformat(_normalise_iso_fraction(value.replace("Z", "+00:00")))
    except ValueError as error:
        raise ValueError(f"V4 supplemental evidence: invalid {label}: {error}") from error
    _require(parsed.tzinfo is not None, f"{label} has no timezone")
    return parsed


def _close(actual: Any, expected: Any, label: str) -> None:
    left, right = _number(actual, label), _number(expected, label + " expected")
    _require(math.isclose(left, right, rel_tol=1e-12, abs_tol=1e-12),
             f"{label} differs: {left} != {right}")


def _percentile(values: Iterable[float], probability: float) -> float:
    ordered = sorted(float(value) for value in values)
    _require(bool(ordered), "cannot compute percentile of an empty sample")
    position = probability * (len(ordered) - 1)
    lower, upper = math.floor(position), math.ceil(position)
    if lower == upper:
        return ordered[lower]
    weight = position - lower
    return ordered[lower] * (1 - weight) + ordered[upper] * weight


def _distribution(values: Iterable[float]) -> dict[str, float | int]:
    ordered = sorted(float(value) for value in values)
    _require(bool(ordered) and all(math.isfinite(one) and one > 0 for one in ordered),
             "latency samples must be positive finite values")
    return {
        "count": len(ordered), "min": ordered[0], "p50": _percentile(ordered, .50),
        "p95": _percentile(ordered, .95), "p99": _percentile(ordered, .99),
        "max": ordered[-1], "mean": sum(ordered) / len(ordered),
    }


def _validate_distribution(value: Any, samples: list[float], label: str) -> None:
    summary = _fields(value, {"count", "min", "p50", "p95", "p99", "max", "mean"}, label)
    expected = _distribution(samples)
    _require(summary["count"] == expected["count"], f"{label}.count differs")
    for name in ("min", "p50", "p95", "p99", "max", "mean"):
        _close(summary[name], expected[name], f"{label}.{name}")


def _parse_go_string_slice(source: str, name: str) -> list[str]:
    matches = list(re.finditer(rf"(?ms)^var\s+{re.escape(name)}\s*=\s*\[\]string\s*\{{(.*?)\}}", source))
    _require(len(matches) == 1, f"Go source must declare {name} exactly once")
    result: list[str] = []
    body = matches[0].group(1)
    literals = list(re.finditer(r'"(?:[^"\\]|\\.)*"', body))
    position = 0
    for index, match in enumerate(literals):
        gap = body[position:match.start()]
        expected_gap = r"\s*" if index == 0 else r"\s*,\s*"
        _require(re.fullmatch(expected_gap, gap) is not None,
                 f"cannot safely parse separators in {name}")
        decoded = json.loads(match.group(0))
        _require(isinstance(decoded, str) and decoded and not decoded.startswith("/") and "\\" not in decoded,
                 f"unsafe path in {name}")
        result.append(decoded)
        position = match.end()
    _require(re.fullmatch(r"\s*,?\s*", body[position:]) is not None,
             f"cannot safely parse trailing syntax in {name}")
    _require(result and len(result) == len(set(result)), f"{name} is empty or repeats a path")
    return result


def _source_declarations(root: Path, relative: Path, roots_name: str,
                         files_name: str) -> tuple[list[str], list[str]]:
    source = _read_regular(root / relative, 2 << 20).decode("utf-8")
    return _parse_go_string_slice(source, roots_name), _parse_go_string_slice(source, files_name)


def _walk_source_paths(root: Path, roots: Iterable[str], explicit: Iterable[str],
                       suffixes: tuple[str, ...]) -> list[Path]:
    result: list[Path] = []
    for relative in roots:
        directory = root / relative
        try:
            info = os.lstat(directory)
        except OSError as error:
            raise ValueError(f"V4 supplemental evidence: source root {relative}: {error}") from error
        _require(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode),
                 f"source root {relative} is invalid")
        count = 0
        for walk_root, directories, files in os.walk(directory, followlinks=False):
            for dirname in directories:
                child = Path(walk_root) / dirname
                _require(not child.is_symlink(), f"source directory {child} is a symlink")
            for filename in files:
                path = Path(walk_root) / filename
                if path.suffix not in suffixes:
                    continue
                _read_regular(path, 16 << 20)
                result.append(path)
                count += 1
        _require(count > 0, f"source root {relative} contains no selected source")
    for relative in explicit:
        path = root / relative
        _read_regular(path, 16 << 20)
        result.append(path)
    normalized = sorted(result, key=lambda path: path.relative_to(root).as_posix())
    relative_names = [path.relative_to(root).as_posix() for path in normalized]
    _require(len(relative_names) == len(set(relative_names)), "source scope repeats a path")
    return normalized


def _path_zero_digest(root: Path, paths: Iterable[Path]) -> str:
    digest = hashlib.sha256()
    for path in paths:
        relative = path.relative_to(root).as_posix()
        digest.update(relative.encode())
        digest.update(b"\0")
        digest.update(_read_regular(path, 16 << 20))
        digest.update(b"\0")
    return digest.hexdigest()


def _concurrency_source_scope(root: Path) -> tuple[str, list[Path]]:
    roots, explicit = _source_declarations(
        root, CONCURRENCY_SOURCE_REL, "boundSourceRoots", "boundSourceFiles")
    paths = _walk_source_paths(root, roots, explicit, (".go", ".sql"))
    return _path_zero_digest(root, paths), paths


def _oracle_repository_source_scope(root: Path) -> tuple[str, list[Path]]:
    roots, explicit = _source_declarations(
        root, ORACLE_SOURCE_REL, "repositorySourceRoots", "repositorySourceFiles")
    paths = _walk_source_paths(root, roots, explicit, (".go",))
    digest = hashlib.sha256()
    for path in paths:
        relative = path.relative_to(root).as_posix().encode()
        raw = _read_regular(path, 16 << 20)
        digest.update(struct.pack(">Q", len(relative)))
        digest.update(relative)
        digest.update(struct.pack(">Q", len(raw)))
        digest.update(raw)
    return digest.hexdigest(), paths


def _oracle_package_digest(root: Path) -> str:
    names = ["cold.go", "oracle.go", "sorter.go", "source.go", "types.go"]
    digest = hashlib.sha256()
    for name in names:
        raw = _read_regular(root / "evaluation/v4oracle" / name, 16 << 20)
        encoded = name.encode()
        digest.update(struct.pack(">Q", len(encoded)))
        digest.update(encoded)
        digest.update(struct.pack(">Q", len(raw)))
        digest.update(raw)
    return digest.hexdigest()


def _generalized_source_scope(root: Path) -> tuple[str, int, list[Path]]:
    _, concurrency = _concurrency_source_scope(root)
    _, oracle = _oracle_repository_source_scope(root)
    distribution = _walk_source_paths(root,
        ["evaluation/cmd/v4-distribution", "evaluation/v4distribution"], [], (".go",))
    paths_by_name = {path.relative_to(root).as_posix(): path
                     for path in concurrency + oracle + distribution}
    validator = root / VALIDATOR_REL
    _read_regular(validator, 4 << 20)
    paths_by_name[VALIDATOR_REL.as_posix()] = validator
    for relative in (
        "evaluation/v4-concurrency/README.md",
        "evaluation/v4-concurrency/catalog.yaml",
        "evaluation/v4-concurrency/compose.yaml",
        "evaluation/v4-concurrency/template.json",
        "paper/tkde/test_v4_supplemental_evidence.py",
    ):
        path = root / relative
        _read_regular(path, 4 << 20)
        paths_by_name[relative] = path
    paths = [paths_by_name[name] for name in sorted(paths_by_name)]
    return _path_zero_digest(root, paths), len(paths), paths


def _memory_source_declarations(files: dict[str, bytes], relative: Path,
                                roots_name: str, files_name: str) -> tuple[list[str], list[str]]:
    name = relative.as_posix()
    _require(name in files, f"historical source archive omits {name}")
    try:
        source = files[name].decode("utf-8")
    except UnicodeDecodeError as error:
        raise ValueError(f"V4 supplemental evidence: archived {name} is not UTF-8") from error
    return _parse_go_string_slice(source, roots_name), _parse_go_string_slice(source, files_name)


def _memory_walk_source_names(files: dict[str, bytes], roots: Iterable[str],
                              explicit: Iterable[str], suffixes: tuple[str, ...]) -> list[str]:
    result: list[str] = []
    for relative in roots:
        parts = PurePosixPath(relative).parts
        _require(parts and all(part not in {"", ".", ".."} for part in parts),
                 f"unsafe archived source root {relative!r}")
        prefix = relative.rstrip("/") + "/"
        selected = sorted(
            name for name in files
            if name.startswith(prefix) and PurePosixPath(name).suffix in suffixes
        )
        _require(bool(selected), f"archived source root {relative} contains no selected source")
        result.extend(selected)
    for relative in explicit:
        parts = PurePosixPath(relative).parts
        _require(parts and all(part not in {"", ".", ".."} for part in parts) and relative in files,
                 f"archived explicit source {relative!r} is invalid")
        result.append(relative)
    normalized = sorted(result)
    _require(len(normalized) == len(set(normalized)), "archived source scope repeats a path")
    return normalized


def _memory_path_zero_digest(files: dict[str, bytes], names: Iterable[str]) -> str:
    digest = hashlib.sha256()
    for name in names:
        digest.update(name.encode())
        digest.update(b"\0")
        digest.update(files[name])
        digest.update(b"\0")
    return digest.hexdigest()


def _historical_source_snapshot(provenance_raw: bytes, archive_raw: bytes) -> dict[str, Any]:
    """Safely reconstruct every source binding recorded by the July campaign."""

    provenance = _decode_json(provenance_raw, "supplemental historical source provenance")
    _fields(provenance, {
        "schema_version", "campaign_id", "git_commit", "git_tree", "archive",
        "archive_sha256", "archive_generation", "source_scope_sha256",
        "source_scope_files", "source_paths_sha256", "source_scope_algorithm",
        "source_selection",
    }, "supplemental historical source provenance")
    _require(
        _integer(provenance["schema_version"], "historical source schema") == 1 and
        provenance["campaign_id"] == EXPECTED_HISTORICAL_CAMPAIGN and
        provenance["git_commit"] == EXPECTED_HISTORICAL_COMMIT and
        provenance["git_tree"] == EXPECTED_HISTORICAL_TREE and
        provenance["archive"] == HISTORICAL_ARCHIVE_REL.as_posix() and
        provenance["archive_generation"] == HISTORICAL_ARCHIVE_GENERATION and
        provenance["source_scope_algorithm"] == MANIFEST_SOURCE_ALGORITHM and
        provenance["source_selection"] == HISTORICAL_SOURCE_SELECTION,
        "supplemental historical source identity/commit/algorithm is invalid",
    )
    _require(0 < len(archive_raw) <= 4 << 20,
             "supplemental historical source archive exceeds its compressed bound")
    _require(archive_raw[:10] == b"\x1f\x8b\x08\x00\x00\x00\x00\x00\x02\x03",
             "supplemental historical source archive lacks the deterministic gzip header")
    archive_sha = _sha(archive_raw)
    _require(
        archive_sha == _digest(provenance["archive_sha256"], "historical archive") ==
        EXPECTED_HISTORICAL_ARCHIVE_SHA256,
        "supplemental historical source archive digest is stale",
    )
    _require(
        _digest(provenance["source_scope_sha256"], "historical generalized source") ==
        EXPECTED_HISTORICAL_SCOPE_SHA256 and
        _integer(provenance["source_scope_files"], "historical source files", minimum=1) ==
        EXPECTED_HISTORICAL_SCOPE_FILES and
        _digest(provenance["source_paths_sha256"], "historical source paths") ==
        EXPECTED_HISTORICAL_PATHS_SHA256,
        "supplemental historical source provenance differs",
    )

    files: dict[str, bytes] = {}
    directories: set[str] = set()
    members: set[str] = set()
    member_count = 0
    total_bytes = 0
    pax_headers: dict[str, str] = {}
    try:
        with tarfile.open(fileobj=io.BytesIO(archive_raw), mode="r|gz") as archive:
            for member in archive:
                member_count += 1
                _require(member_count <= 256, "supplemental historical archive has too many members")
                name = member.name
                path = PurePosixPath(name)
                _require(
                    isinstance(name, str) and name and name == path.as_posix() and
                    not path.is_absolute() and "\\" not in name and "\0" not in name and
                    all(part not in {"", ".", ".."} for part in path.parts) and
                    name not in members,
                    f"unsafe or duplicate supplemental historical source member {name!r}",
                )
                members.add(name)
                _require(
                    member.uid == 0 and member.gid == 0 and member.uname == "root" and
                    member.gname == "root" and member.mtime == EXPECTED_HISTORICAL_MTIME and
                    member.pax_headers == {"comment": EXPECTED_HISTORICAL_COMMIT} and
                    not member.linkname,
                    f"supplemental historical source member {name!r} metadata is not deterministic",
                )
                if member.isdir():
                    _require(member.size == 0 and member.mode == 0o755,
                             f"supplemental historical directory {name!r} metadata is invalid")
                    directories.add(name)
                    continue
                _require(member.isreg() and member.mode in {0o644, 0o755},
                         f"supplemental historical member {name!r} is not a regular file")
                _require(0 < member.size <= 16 << 20,
                         f"supplemental historical member {name!r} exceeds its bound")
                total_bytes += member.size
                _require(total_bytes <= 16 << 20,
                         "supplemental historical archive exceeds its decompressed bound")
                extracted = archive.extractfile(member)
                _require(extracted is not None,
                         f"cannot read supplemental historical source member {name!r}")
                raw = extracted.read(member.size + 1)
                _require(len(raw) == member.size,
                         f"supplemental historical source member {name!r} changed size")
                files[name] = raw
            pax_headers = dict(archive.pax_headers)
    except (tarfile.TarError, OSError, EOFError) as error:
        raise ValueError(f"V4 supplemental evidence: invalid historical source archive: {error}") from error
    _require(pax_headers == {"comment": EXPECTED_HISTORICAL_COMMIT},
             "supplemental historical archive does not bind the exact Git commit")

    expected_directories: set[str] = set()
    for name in files:
        parent = PurePosixPath(name).parent
        while parent != PurePosixPath("."):
            expected_directories.add(parent.as_posix())
            parent = parent.parent
    _require(
        member_count == EXPECTED_HISTORICAL_ARCHIVE_MEMBERS and
        directories == expected_directories and members == directories | set(files),
        "supplemental historical archive directory/member set is not exact",
    )

    concurrency_roots, concurrency_explicit = _memory_source_declarations(
        files, CONCURRENCY_SOURCE_REL, "boundSourceRoots", "boundSourceFiles")
    concurrency_names = _memory_walk_source_names(
        files, concurrency_roots, concurrency_explicit, (".go", ".sql"))
    oracle_roots, oracle_explicit = _memory_source_declarations(
        files, ORACLE_SOURCE_REL, "repositorySourceRoots", "repositorySourceFiles")
    oracle_names = _memory_walk_source_names(files, oracle_roots, oracle_explicit, (".go",))
    distribution_names = _memory_walk_source_names(
        files, ["evaluation/cmd/v4-distribution", "evaluation/v4distribution"], [], (".go",))

    generalized = {name: None for name in concurrency_names + oracle_names + distribution_names}
    for name in (
        VALIDATOR_REL.as_posix(),
        "evaluation/v4-concurrency/README.md",
        "evaluation/v4-concurrency/catalog.yaml",
        "evaluation/v4-concurrency/compose.yaml",
        "evaluation/v4-concurrency/template.json",
        "paper/tkde/test_v4_supplemental_evidence.py",
    ):
        _require(name in files, f"supplemental historical archive omits fixed source {name}")
        generalized[name] = None
    generalized_names = sorted(generalized)
    _require(set(generalized_names) == set(files),
             "supplemental historical archive differs from its reconstructed source scope")

    path_digest = hashlib.sha256()
    for name in generalized_names:
        path_digest.update(name.encode())
        path_digest.update(b"\0")
    generalized_sha = _memory_path_zero_digest(files, generalized_names)
    concurrency_sha = _memory_path_zero_digest(files, concurrency_names)

    oracle_digest = hashlib.sha256()
    for name in oracle_names:
        encoded = name.encode()
        oracle_digest.update(struct.pack(">Q", len(encoded)))
        oracle_digest.update(encoded)
        oracle_digest.update(struct.pack(">Q", len(files[name])))
        oracle_digest.update(files[name])
    package_digest = hashlib.sha256()
    for basename in ("cold.go", "oracle.go", "sorter.go", "source.go", "types.go"):
        name = "evaluation/v4oracle/" + basename
        raw = files[name]
        encoded = basename.encode()
        package_digest.update(struct.pack(">Q", len(encoded)))
        package_digest.update(encoded)
        package_digest.update(struct.pack(">Q", len(raw)))
        package_digest.update(raw)

    _require(
        len(generalized_names) == EXPECTED_HISTORICAL_SCOPE_FILES and
        generalized_sha == provenance["source_scope_sha256"] == EXPECTED_HISTORICAL_SCOPE_SHA256 and
        path_digest.hexdigest() == provenance["source_paths_sha256"] ==
        EXPECTED_HISTORICAL_PATHS_SHA256 and
        len(concurrency_names) == EXPECTED_HISTORICAL_CONCURRENCY_FILES and
        concurrency_sha == EXPECTED_HISTORICAL_CONCURRENCY_SHA256 and
        len(oracle_names) == EXPECTED_HISTORICAL_ORACLE_REPOSITORY_FILES and
        oracle_digest.hexdigest() == EXPECTED_HISTORICAL_ORACLE_REPOSITORY_SHA256 and
        package_digest.hexdigest() == EXPECTED_HISTORICAL_ORACLE_PACKAGE_SHA256,
        "supplemental historical source bindings are not reproducible",
    )
    return {
        "mode": "historical_source_snapshot",
        "git_commit": EXPECTED_HISTORICAL_COMMIT,
        "git_tree": EXPECTED_HISTORICAL_TREE,
        "archive_sha256": archive_sha,
        "source_scope_sha256": generalized_sha,
        "source_scope_files": len(generalized_names),
        "source_scope_paths": generalized_names,
        "source_paths_sha256": path_digest.hexdigest(),
        "concurrency_source_sha256": concurrency_sha,
        "concurrency_source_files": len(concurrency_names),
        "oracle_repository_sha256": oracle_digest.hexdigest(),
        "oracle_repository_files": len(oracle_names),
        "oracle_package_sha256": package_digest.hexdigest(),
        "archive_member_count": member_count,
        "archive_uncompressed_source_bytes": total_bytes,
    }


def _current_source_relation(root: Path, historical_sha256: str) -> dict[str, Any]:
    try:
        current_sha, current_files, _ = _generalized_source_scope(root)
    except ValueError as error:
        return {
            "status": "unavailable", "current_source_sha256": None,
            "current_source_files": None, "historical_source_sha256": historical_sha256,
            "matches_historical": False, "reason": str(error),
        }
    matches = current_sha == historical_sha256
    return {
        "status": "match" if matches else "diverged",
        "current_source_sha256": current_sha,
        "current_source_files": current_files,
        "historical_source_sha256": historical_sha256,
        "matches_historical": matches,
    }


def _load_manifest(root: Path, evidence_dir: Path | str | None,
                   historical_source: dict[str, Any] | None = None
                   ) -> tuple[dict[str, Any], dict[str, bytes]]:
    directory = root / EVIDENCE_REL if evidence_dir is None else Path(evidence_dir)
    try:
        info = os.lstat(directory)
    except OSError as error:
        raise ValueError(f"V4 supplemental evidence: cannot stat evidence directory: {error}") from error
    _require(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode),
             "evidence directory is missing or a symlink")
    actual: set[str] = set()
    with os.scandir(directory) as entries:
        for entry in entries:
            _require(entry.is_file(follow_symlinks=False),
                     f"unexpected non-file evidence entry {entry.name}")
            actual.add(entry.name)
    _require(actual == EXPECTED_LOCAL_FILES,
             f"evidence directory set differs: {sorted(actual ^ EXPECTED_LOCAL_FILES)}")

    manifest = _decode_json(_read_regular(directory / "manifest.json", MAXIMUMS["manifest.json"]),
                            "supplemental manifest")
    _fields(manifest, {"schema_version", "status", "source_scope", "artifacts"}, "manifest")
    _require(_integer(manifest["schema_version"], "manifest.schema_version") == 1 and
             manifest["status"] == "complete_supplemental_campaign",
             "manifest schema/status is invalid")
    scope = _fields(manifest["source_scope"], {"sha256", "files", "algorithm"}, "manifest.source_scope")
    _digest(scope["sha256"], "manifest source scope")
    _integer(scope["files"], "manifest source files", minimum=1)
    _require(scope["algorithm"] == MANIFEST_SOURCE_ALGORITHM, "manifest source algorithm differs")
    artifacts = manifest["artifacts"]
    _require(isinstance(artifacts, dict) and set(artifacts) == EXPECTED_ARTIFACTS,
             "manifest artifact set is not exact")

    retained: dict[str, bytes] = {}
    for relative in sorted(EXPECTED_ARTIFACTS):
        if relative.startswith(EVIDENCE_REL.as_posix() + "/"):
            path = directory / Path(relative).name
            maximum = MAXIMUMS[path.name]
        else:
            path = root / relative
            maximum = MAXIMUMS[relative]
        raw = _read_regular(path, maximum)
        _require(_sha(raw) == _digest(artifacts[relative], f"artifact digest {relative}"),
                 f"artifact SHA-256 differs for {relative}")
        retained[relative] = raw
    if historical_source is None:
        expected_sha, expected_files, _ = _generalized_source_scope(root)
    else:
        expected_sha = historical_source["source_scope_sha256"]
        expected_files = historical_source["source_scope_files"]
    _require(scope["sha256"] == expected_sha and scope["files"] == expected_files,
             "generalized source scope is stale")
    return manifest, retained


def _framed_zero_digest(domain: bytes, fields: Iterable[str]) -> str:
    digest = hashlib.sha256(domain)
    for field in fields:
        digest.update(field.encode())
        digest.update(b"\0")
    return digest.hexdigest()


def _validate_no_secrets(value: Any, path: str = "environment") -> None:
    forbidden_key = re.compile(r"(?:dsn|token|password|secret|credential|private[_-]?key)", re.IGNORECASE)
    if isinstance(value, dict):
        for key, child in value.items():
            _require(forbidden_key.search(key) is None, f"{path} contains forbidden secret-like key {key!r}")
            _validate_no_secrets(child, path + "." + key)
    elif isinstance(value, list):
        for index, child in enumerate(value):
            _validate_no_secrets(child, f"{path}[{index}]")
    elif isinstance(value, str):
        lowered = value.lower()
        _require("postgres://" not in lowered and "postgresql://" not in lowered and
                 "password=" not in lowered and "bearer " not in lowered,
                 f"{path} contains credential-like text")


def _image_digest(value: Any, label: str) -> str:
    _require(isinstance(value, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", value) is not None,
             f"{label} is not a Docker sha256 image ID")
    return value


def _validate_environment(value: dict[str, Any], results_sha256: str) -> dict[str, Any]:
    environment = _fields(value, {
        "schema_version", "captured_at", "host", "software", "base_v4", "datasets",
    }, "environment")
    _require(_integer(environment["schema_version"], "environment.schema_version") == 2,
             "environment schema_version is not 2")
    _timestamp(environment["captured_at"], "environment.captured_at")
    host = _fields(environment["host"], {
        "kernel", "architecture", "cpu_model", "logical_cpus", "memory_bytes",
    }, "environment.host")
    _require(all(isinstance(host[name], str) and host[name] for name in
                 ("kernel", "architecture", "cpu_model")) and
             _integer(host["logical_cpus"], "environment logical CPUs", minimum=1) > 0 and
             _integer(host["memory_bytes"], "environment memory", minimum=1 << 30) > 0,
             "environment host/kernel/CPU identity is incomplete")
    software = _fields(environment["software"], {
        "go_version", "postgres_version", "images", "concurrency_gateway_binary_sha256",
    },
                       "environment.software")
    _require(isinstance(software["go_version"], str) and software["go_version"].startswith("go1.") and
             isinstance(software["postgres_version"], str) and "PostgreSQL 16" in software["postgres_version"],
             "environment Go/PostgreSQL versions are invalid")
    images = _fields(software["images"], {
        "go_build", "postgres", "concurrency_gateway", "concurrency_gateway_peer",
        "concurrency_oa",
    }, "environment.software.images")
    for name, image_id in images.items():
        _image_digest(image_id, "environment image " + name)
    _digest(software["concurrency_gateway_binary_sha256"],
            "environment concurrency Gateway binary")
    base = _fields(environment["base_v4"], {"campaign_id", "results_sha256"}, "environment.base_v4")
    _require(base["campaign_id"] == "taskgate-v4-full-20260730t070232z" and
             base["results_sha256"] == results_sha256,
             "environment does not bind the retained base V4 campaign/results")
    datasets = _fields(environment["datasets"], {"business", "control", "concurrency"},
                       "environment.datasets")
    business = _fields(datasets["business"], {
        "identity_sha256", "name", "snapshot_id", "scale_orders_rows", "scale_lineitem_rows", "frozen",
    }, "environment.datasets.business")
    _digest(business["identity_sha256"], "Business dataset identity")
    _require(isinstance(business["name"], str) and business["name"] and
             business["snapshot_id"] == "exposure-scale-2026-v4-narrow-1" and
             business["scale_orders_rows"] == 50_000 and business["scale_lineitem_rows"] == 250_000 and
             business["frozen"] is True, "Business frozen dataset identity is invalid")
    control = _fields(datasets["control"], {
        "identity_sha256", "name", "catalog_sha256", "dictionary_set_sha256", "exposure_profile",
    }, "environment.datasets.control")
    for name in ("identity_sha256", "catalog_sha256", "dictionary_set_sha256"):
        _digest(control[name], "Control dataset " + name)
    _require(isinstance(control["name"], str) and control["name"] and
             control["exposure_profile"] == "taskgate-exposure-v4",
             "Control dataset identity/profile is invalid")
    concurrency = _fields(datasets["concurrency"], {
        "project", "snapshot_id", "expense_detail_rows", "catalog_sha256",
        "gateway_replicas", "per_gateway_control_pool", "frozen",
    }, "environment.datasets.concurrency")
    _require(concurrency["project"] == "taskgate-v4-concurrency-final" and
             concurrency["snapshot_id"] == "travel-demo-2026-v1" and
             concurrency["expense_detail_rows"] == 10 and
             concurrency["catalog_sha256"] == EXPECTED_CONCURRENCY_CATALOG_SHA256 and
             concurrency["gateway_replicas"] == 2 and
             concurrency["per_gateway_control_pool"] == 10 and
             concurrency["frozen"] is True,
             "concurrency fixture/project identity is invalid")
    _validate_no_secrets(environment)
    return environment


def _bitmap_metric(value: Any, label: str) -> dict[str, Any]:
    required = {
        "cardinality", "container_count", "portable_bitmap_bytes", "digest",
        "portable_round_trip_verified", "round_trip_digest", "has_ordinals",
    }
    metric = _fields(value, required, label, {"minimum_ordinal", "maximum_ordinal"})
    cardinality = _integer(metric["cardinality"], label + ".cardinality", minimum=0)
    containers = _integer(metric["container_count"], label + ".container_count", minimum=0)
    portable = _integer(metric["portable_bitmap_bytes"], label + ".portable_bitmap_bytes", minimum=0)
    digest = _digest(metric["digest"], label + ".digest")
    _require(metric["portable_round_trip_verified"] is True and
             metric["round_trip_digest"] == digest,
             f"{label} did not pass full portable serialize/parse/equality/digest round trip")
    _require(isinstance(metric["has_ordinals"], bool), f"{label}.has_ordinals is not Boolean")
    if cardinality == 0:
        _require(not metric["has_ordinals"] and containers == 0 and portable == 0 and
                 "minimum_ordinal" not in metric and "maximum_ordinal" not in metric,
                 f"{label} empty representation is incoherent")
    else:
        _require(metric["has_ordinals"] and containers > 0 and portable > 0 and
                 "maximum_ordinal" in metric, f"{label} nonempty representation is incomplete")
        minimum = _integer(metric.get("minimum_ordinal", 0), label + ".minimum_ordinal",
                           minimum=0, maximum=2**32 - 1)
        maximum = _integer(metric["maximum_ordinal"], label + ".maximum_ordinal",
                           minimum=0, maximum=2**32 - 1)
        _require(minimum <= maximum, f"{label} ordinal bounds are reversed")
    return metric


def _bitmap_minimum(metric: dict[str, Any]) -> int:
    return int(metric.get("minimum_ordinal", 0))


def _distribution_cell_digest(cell: dict[str, Any]) -> str:
    fields = [
        "taskgate-v4-bitmap-distribution-v2",
        cell["distribution"],
        str(cell["target_overlap_percent"]),
        str(cell["effect"]["cardinality"]), cell["effect"]["digest"],
        str(cell["effect"]["portable_round_trip_verified"]).lower(), cell["effect"]["round_trip_digest"],
        str(cell["ledger_before"]["cardinality"]), cell["ledger_before"]["digest"],
        str(cell["ledger_before"]["portable_round_trip_verified"]).lower(), cell["ledger_before"]["round_trip_digest"],
        str(cell["novel_delta"]["cardinality"]), cell["novel_delta"]["digest"],
        str(cell["novel_delta"]["portable_round_trip_verified"]).lower(), cell["novel_delta"]["round_trip_digest"],
        str(cell["ledger_after"]["cardinality"]), cell["ledger_after"]["digest"],
        str(cell["ledger_after"]["portable_round_trip_verified"]).lower(), cell["ledger_after"]["round_trip_digest"],
        str(cell["replay_delta"]["cardinality"]), cell["replay_delta"]["digest"],
        str(cell["replay_delta"]["portable_round_trip_verified"]).lower(), cell["replay_delta"]["round_trip_digest"],
        cell["observation_sha256"],
    ]
    return _framed_zero_digest(b"TASKGATE-V4-DISTRIBUTION-CELL-V2\0", fields)


def _distribution_observation_digest(effect_digest: str, cardinality: int) -> str:
    digest = hashlib.sha256(b"TASKGATE-V4-DISTRIBUTION-OBSERVATION-V2\0")
    digest.update(effect_digest.encode())
    digest.update(b"\0")
    digest.update(str(cardinality).encode())
    return digest.hexdigest()


def _validate_latency(value: Any, runs: int, label: str) -> dict[str, Any]:
    evidence = _fields(value, {"samples_ms", "summary"}, label)
    raw = _list(evidence["samples_ms"], label + ".samples_ms", length=runs)
    samples = [_number(one, label + ".sample", minimum=0) for one in raw]
    _require(all(one > 0 for one in samples), f"{label} contains a zero latency")
    _validate_distribution(evidence["summary"], samples, label + ".summary")
    return evidence


def _validate_distribution_report(value: dict[str, Any]) -> dict[str, Any]:
    top = _fields(value, {
        "schema_version", "status", "generator_version", "scope", "started_at", "finished_at",
        "configuration", "runtime", "metric_semantics", "cells", "matrix_sha256",
        "acceptance_eligible",
    }, "distribution report")
    _require(top["schema_version"] == 2 and top["status"] == "complete_measured_kernel" and
             top["generator_version"] == "taskgate-v4-bitmap-distribution-v2",
             "distribution report identity/status is invalid")
    _require(top["scope"] ==
             "ordinal BitmapSet kernel only; excludes Gateway, PostgreSQL, networking, encryption, CAS, and result persistence",
             "distribution report overstates its kernel scope")
    started, finished = _timestamp(top["started_at"], "distribution.started_at"), _timestamp(
        top["finished_at"], "distribution.finished_at")
    _require(finished >= started, "distribution timestamps are reversed")
    cfg = _fields(top["configuration"], {
        "cardinality", "runs", "cluster_count", "random_seed", "replay_lookups_per_run",
        "max_peak_heap_bytes",
    }, "distribution.configuration")
    _require(cfg == {
        "cardinality": 1_035_000,
        "runs": _integer(cfg["runs"], "distribution runs", minimum=50, maximum=100),
        "cluster_count": 128,
        "random_seed": 0x6D2B79F5,
        "replay_lookups_per_run": 4096,
        "max_peak_heap_bytes": 512 << 20,
    }, "distribution configuration is not the pinned acceptance contract")
    runs = cfg["runs"]
    _require(top["acceptance_eligible"] is True, "distribution report is not acceptance eligible")
    runtime = _fields(top["runtime"], {"go_version", "goos", "goarch", "cpus"}, "distribution.runtime")
    _require(all(isinstance(runtime[name], str) and runtime[name] for name in ("go_version", "goos", "goarch")) and
             _integer(runtime["cpus"], "distribution CPUs", minimum=1) > 0,
             "distribution runtime identity is incomplete")
    semantics = _fields(top["metric_semantics"], {
        "andnot_or_latency_ms", "replay_digest_lookup_latency_ms", "portable_bitmap_bytes",
        "portable_round_trip_verified", "peak_heap_delta_bytes", "total_alloc_delta_bytes",
        "construction_and_encode_ms",
    }, "distribution.metric_semantics")
    _require(all(isinstance(one, str) and one for one in semantics.values()),
             "distribution metric semantics are empty")

    cells = _list(top["cells"], "distribution.cells", length=12)
    order = [(name, overlap) for name in ("dense", "clustered", "random_sparse")
             for overlap in (0, 50, 90, 100)]
    cell_digests: list[str] = []
    effect_by_shape: dict[str, dict[str, Any]] = {}
    observation_by_shape: dict[str, str] = {}
    memory_peaks: list[int] = []
    andnot_or_p95_ms: list[float] = []
    for index, (value_cell, expected_identity) in enumerate(zip(cells, order, strict=True)):
        label = f"distribution.cells[{index}]"
        cell = _fields(value_cell, {
            "distribution", "target_overlap_percent", "observed_overlap_percent", "effect",
            "ledger_before", "novel_delta", "ledger_after", "replay_delta",
            "observation_sha256", "replay_observation_sha256", "replay_matched",
            "andnot_or_latency_ms", "replay_digest_lookup_latency_ms", "replay_lookups_per_run",
            "construction_and_encode_ms", "memory", "deterministic_cell_sha256",
        }, label)
        shape, target = expected_identity
        observed_target = _integer(cell["target_overlap_percent"], label + ".target_overlap_percent",
                                   minimum=0, maximum=100)
        _require((cell["distribution"], observed_target) == expected_identity,
                 f"{label} is out of canonical matrix order")
        _close(cell["observed_overlap_percent"], target, label + ".observed_overlap_percent")
        effect = _bitmap_metric(cell["effect"], label + ".effect")
        before = _bitmap_metric(cell["ledger_before"], label + ".ledger_before")
        novel = _bitmap_metric(cell["novel_delta"], label + ".novel_delta")
        after = _bitmap_metric(cell["ledger_after"], label + ".ledger_after")
        replay = _bitmap_metric(cell["replay_delta"], label + ".replay_delta")
        overlap_count = 1_035_000 * target // 100
        _require(effect["cardinality"] == 1_035_000 and before["cardinality"] == overlap_count and
                 novel["cardinality"] == 1_035_000 - overlap_count and
                 after["cardinality"] == 1_035_000 and replay["cardinality"] == 0,
                 f"{label} cardinalities do not implement exact overlap/ANDNOT/OR/replay")
        _require(after == effect, f"{label} ledger-after differs from effect although prior is a subset")
        if target == 0:
            _require(before["cardinality"] == 0, f"{label} zero-overlap prior is nonempty")
        if target == 100:
            _require(before == effect, f"{label} full-overlap prior differs from effect")
        expected_effect = EXPECTED_DISTRIBUTION_EFFECTS[shape]
        _require(effect["container_count"] == expected_effect["container_count"] and
                 effect["portable_bitmap_bytes"] == expected_effect["portable_bitmap_bytes"] and
                 effect["digest"] == expected_effect["digest"] and
                 _bitmap_minimum(effect) == expected_effect["minimum_ordinal"] and
                 effect["maximum_ordinal"] == expected_effect["maximum_ordinal"],
                 f"{label} effect is not the pinned {shape} physical distribution")
        if shape in effect_by_shape:
            _require(effect == effect_by_shape[shape], f"{shape} effect changes with overlap")
        else:
            effect_by_shape[shape] = effect
        observation = _digest(cell["observation_sha256"], label + ".observation")
        _require(observation == _distribution_observation_digest(effect["digest"], 1_035_000) and
                 cell["replay_observation_sha256"] == observation and cell["replay_matched"] is True,
                 f"{label} replay digest lookup is incoherent")
        if shape in observation_by_shape:
            _require(observation_by_shape[shape] == observation, f"{shape} observation changes with overlap")
        else:
            observation_by_shape[shape] = observation
        novel_latency = _validate_latency(
            cell["andnot_or_latency_ms"], runs, label + ".andnot_or_latency_ms")
        andnot_or_p95_ms.append(float(novel_latency["summary"]["p95"]))
        _validate_latency(cell["replay_digest_lookup_latency_ms"], runs,
                          label + ".replay_digest_lookup_latency_ms")
        _require(cell["replay_lookups_per_run"] == 4096 and
                 _number(cell["construction_and_encode_ms"], label + ".construction", minimum=0) > 0,
                 f"{label} construction/replay batch evidence is invalid")
        memory = _fields(cell["memory"], {
            "start_heap_alloc_bytes", "end_heap_alloc_bytes", "peak_heap_alloc_bytes",
            "peak_heap_inuse_bytes", "peak_heap_sys_bytes", "peak_heap_delta_bytes",
            "total_alloc_delta_bytes",
        }, label + ".memory")
        start = _integer(memory["start_heap_alloc_bytes"], label + ".memory.start", minimum=0)
        end = _integer(memory["end_heap_alloc_bytes"], label + ".memory.end", minimum=0)
        peak = _integer(memory["peak_heap_alloc_bytes"], label + ".memory.peak", minimum=1,
                        maximum=512 << 20)
        delta = _integer(memory["peak_heap_delta_bytes"], label + ".memory.delta", minimum=0)
        _require(peak >= start and peak >= end and delta == peak - start and
                 _integer(memory["peak_heap_inuse_bytes"], label + ".memory.inuse", minimum=1) > 0 and
                 _integer(memory["peak_heap_sys_bytes"], label + ".memory.sys", minimum=1) > 0 and
                 _integer(memory["total_alloc_delta_bytes"], label + ".memory.total", minimum=1) > 0,
                 f"{label} heap/workset evidence is incoherent")
        memory_peaks.append(peak)
        expected_cell_digest = _distribution_cell_digest(cell)
        _require(cell["deterministic_cell_sha256"] == expected_cell_digest,
                 f"{label} deterministic cell digest differs")
        cell_digests.append(expected_cell_digest)
    matrix = _framed_zero_digest(b"taskgate-v4-bitmap-distribution-v2\0", cell_digests)
    _require(top["matrix_sha256"] == matrix == EXPECTED_DISTRIBUTION_MATRIX_SHA256,
             "distribution matrix digest is not the pinned V2 matrix")
    _require(len({(one["container_count"], one["portable_bitmap_bytes"], _bitmap_minimum(one),
                   one["maximum_ordinal"]) for one in effect_by_shape.values()}) == 3,
             "the three distribution effects are not physically distinct")
    return {
        "report": top,
        "cell_count": len(cells),
        "runs_per_cell": runs,
        "portable_round_trip_checks": len(cells) * 5,
        "worst_andnot_or_p95_ms": max(andnot_or_p95_ms),
        "max_peak_heap_bytes": max(memory_peaks),
    }


def _exposure_counts(value: Any, label: str) -> dict[str, int]:
    counts = _fields(value, {"release", "influence", "outcome"}, label)
    return {name: _integer(counts[name], f"{label}.{name}", minimum=0)
            for name in ("release", "influence", "outcome")}


def _count_subtract(left: dict[str, int], right: dict[str, int]) -> dict[str, int]:
    return {name: left[name] - right[name] for name in left}


def _safe_id(value: str) -> str:
    result = re.sub(r"[^A-Za-z0-9_-]", "-", value).strip("-")
    return result


def _validate_concurrency_config(value: dict[str, Any]) -> dict[str, Any]:
    cfg = _fields(value, {
        "schema_version", "gateway", "control_dsn_env", "request_timeout_ms",
        "lock_wait_timeout_ms", "provision", "cases",
    }, "concurrency config")
    _require(_integer(cfg["schema_version"], "concurrency config.schema_version") == 1,
             "concurrency config schema is not 1")
    gateway = _fields(cfg["gateway"], {"url", "contender_urls", "token_env"},
                      "concurrency config.gateway")
    _require(isinstance(gateway["url"], str) and gateway["url"] and
             isinstance(gateway["token_env"], str) and gateway["token_env"],
             "concurrency gateway config is incomplete")
    contender_urls = _list(gateway["contender_urls"],
                           "concurrency config.gateway.contender_urls", length=2)
    _require(contender_urls == ["http://127.0.0.1:8082", "http://127.0.0.1:8083"] and
             gateway["url"] == contender_urls[0] and len(set(contender_urls)) == 2,
             "concurrency contender Gateway replicas are not the fixed two-endpoint setup")
    _require(isinstance(cfg["control_dsn_env"], str) and cfg["control_dsn_env"].strip() == cfg["control_dsn_env"] and
             cfg["control_dsn_env"], "concurrency Control DSN env name is invalid")
    request_timeout = _integer(cfg["request_timeout_ms"], "concurrency request timeout", minimum=1)
    lock_timeout = _integer(cfg["lock_wait_timeout_ms"], "concurrency lock timeout", minimum=1)
    _require(lock_timeout < request_timeout, "concurrency lock timeout does not precede request timeout")
    provision = _fields(cfg["provision"], {
        "oa_url", "alice_password_env", "bob_password_env", "data_products", "columns", "scopes",
    }, "concurrency config.provision")
    for name in ("oa_url", "alice_password_env", "bob_password_env"):
        _require(isinstance(provision[name], str) and provision[name], f"concurrency provision {name} is empty")
    products = _list(provision["data_products"], "concurrency data products")
    _require(products and all(isinstance(one, str) and one for one in products) and
             len(products) == len(set(products)), "concurrency products are invalid")
    _require(isinstance(provision["columns"], dict) and set(provision["columns"]) == set(products),
             "concurrency provision columns do not match products")
    for product, columns in provision["columns"].items():
        values = _list(columns, f"columns for {product}")
        _require(values and all(isinstance(one, str) and one for one in values) and
                 len(values) == len(set(values)),
                 f"columns for {product} are invalid")
    _require(isinstance(provision["scopes"], dict) and provision["scopes"],
             "concurrency provision scopes are empty")

    cases = _list(cfg["cases"], "concurrency config.cases", length=4)
    levels: set[int] = set()
    dimensions: set[str] = set()
    case_ids: set[str] = set()
    roots: set[str] = set()
    tasks: set[str] = set()
    for index, raw_case in enumerate(cases):
        label = f"concurrency config.cases[{index}]"
        case = _fields(raw_case, {
            "id", "concurrency", "boundary_dimension", "root_task_id", "prefix_task_id",
            "contender_task_ids", "overflow_task_id", "prefix_plan", "contender_plan",
            "overflow_plan", "before_used", "at_budget",
        }, label)
        _require(isinstance(case["id"], str) and case["id"] and case["id"] not in case_ids,
                 f"{label} ID is empty or duplicate")
        case_ids.add(case["id"])
        level = _integer(case["concurrency"], label + ".concurrency")
        _require(level in {1, 4, 8, 16} and level not in levels, f"{label} concurrency is unsupported or duplicate")
        levels.add(level)
        dimension = case["boundary_dimension"]
        _require(isinstance(dimension, str) and dimension in {"release", "influence", "outcome"},
                 f"{label} boundary is invalid")
        dimensions.add(dimension)
        root = case["root_task_id"]
        _require(isinstance(root, str) and root and root not in roots, f"{label} root task is empty or reused")
        roots.add(root)
        contenders = _list(case["contender_task_ids"], label + ".contenders", length=level)
        operation_tasks = [case["prefix_task_id"], case["overflow_task_id"], *contenders]
        _require(all(isinstance(one, str) and one and one not in tasks for one in operation_tasks) and
                 len(operation_tasks) == len(set(operation_tasks)), f"{label} operation task is empty/reused")
        tasks.update(operation_tasks)
        plans: list[str] = []
        for name in ("prefix_plan", "contender_plan", "overflow_plan"):
            _require(isinstance(case[name], dict) and case[name], f"{label}.{name} is not a plan object")
            plans.append(json.dumps(case[name], sort_keys=True, separators=(",", ":"), ensure_ascii=False))
        _require(len(set(plans)) == 3, f"{label} plans are not distinct")
        before = _exposure_counts(case["before_used"], label + ".before_used")
        budget = _exposure_counts(case["at_budget"], label + ".at_budget")
        delta = _count_subtract(budget, before)
        _require(all(budget[name] > 0 and delta[name] > 0 for name in delta) and
                 delta[dimension] == 1, f"{label} does not encode three-dimensional exact B-1")
    _require(levels == {1, 4, 8, 16} and dimensions == {"release", "influence", "outcome"},
             "concurrency config omits required widths or boundary dimensions")
    return cfg


def _root_head(value: Any, label: str) -> dict[str, Any]:
    head = _fields(value, {"epoch", "limits", "used"}, label,
                   {"release_set_sha256", "influence_set_sha256", "outcome_set_sha256"})
    _integer(head["epoch"], label + ".epoch", minimum=0)
    _exposure_counts(head["limits"], label + ".limits")
    _exposure_counts(head["used"], label + ".used")
    for name in ("release_set_sha256", "influence_set_sha256", "outcome_set_sha256"):
        if name in head:
            _digest(head[name], label + "." + name)
    return head


def _content_counts(value: Any, label: str) -> dict[str, int]:
    counts = _fields(value, {"containers", "sets", "dynamic_facts", "observations"}, label)
    return {name: _integer(counts[name], f"{label}.{name}", minimum=0) for name in counts}


def _validate_concurrency_report(value: dict[str, Any], config: dict[str, Any],
                                 config_raw: bytes, root: Path,
                                 historical_source_sha256: str | None = None
                                 ) -> dict[str, Any]:
    report = _fields(value, {
        "schema_version", "status", "acceptance", "started_at", "finished_at", "configuration",
        "provenance", "metric_notes", "cells", "gates",
    }, "concurrency report", {"errors"})
    _require(_integer(report["schema_version"], "concurrency.schema_version") == 2 and
             report["status"] == "complete_measured_campaign" and
             report["acceptance"] == "pass", "concurrency report identity/status is invalid")
    _require("errors" not in report or report["errors"] == [],
             "passing concurrency report contains execution errors")
    started, finished = _timestamp(report["started_at"], "concurrency.started_at"), _timestamp(
        report["finished_at"], "concurrency.finished_at")
    _require(finished >= started, "concurrency timestamps are reversed")
    summary = _fields(report["configuration"], {
        "gateway_url", "contender_gateway_urls", "contender_gateway_count",
        "per_gateway_control_pool", "request_timeout_ms", "lock_wait_timeout_ms", "case_count",
        "concurrency_levels", "boundary_dimensions",
    }, "concurrency.configuration")
    expected_levels = sorted(case["concurrency"] for case in config["cases"])
    expected_dimensions = sorted(set(case["boundary_dimension"] for case in config["cases"]))
    observed_levels = _list(summary["concurrency_levels"], "concurrency levels", length=4)
    for index, level in enumerate(observed_levels):
        _integer(level, f"concurrency levels[{index}]", minimum=1)
    observed_dimensions = _list(summary["boundary_dimensions"], "concurrency dimensions", length=3)
    _require(all(isinstance(one, str) for one in observed_dimensions),
             "concurrency dimensions contain a non-string value")
    summary_urls = _list(summary["contender_gateway_urls"],
                         "concurrency summary contender Gateway URLs", length=2)
    _require(summary["gateway_url"] == config["gateway"]["url"].rstrip("/") and
             summary_urls == config["gateway"]["contender_urls"] and
             _integer(summary["contender_gateway_count"],
                      "concurrency summary Gateway count") == 2 and
             _integer(summary["per_gateway_control_pool"],
                      "concurrency summary per-Gateway Control pool") == 10 and
             _integer(summary["request_timeout_ms"], "concurrency summary request timeout") ==
             config["request_timeout_ms"] and
             _integer(summary["lock_wait_timeout_ms"], "concurrency summary lock timeout") ==
             config["lock_wait_timeout_ms"] and
             _integer(summary["case_count"], "concurrency summary case count") == 4 and
             observed_levels == expected_levels and observed_dimensions == expected_dimensions,
             "concurrency report summary differs from bound config")
    provenance = _fields(report["provenance"], {"config_sha256", "source_sha256"},
                         "concurrency.provenance")
    if historical_source_sha256 is None:
        expected_source, _ = _concurrency_source_scope(root)
    else:
        expected_source = _digest(
            historical_source_sha256, "historical concurrency source")
    _require(provenance["config_sha256"] == _sha(config_raw) and
             provenance["source_sha256"] == expected_source,
             "concurrency config/source binding is stale")
    notes = _fields(report["metric_notes"], {
        "client_latency_ms", "root_lock_waiters", "inference_boundary", "failure_audit",
        "gateway_replicas",
    }, "concurrency.metric_notes")
    _require(all(isinstance(one, str) and one for one in notes.values()), "concurrency metric notes are empty")

    cells = _list(report["cells"], "concurrency.cells", length=4)
    by_id: dict[str, dict[str, Any]] = {}
    maximum_width = 0
    total_zero_novelty = 0
    total_root_lock_waiters = 0
    for index, raw_cell in enumerate(cells):
        label = f"concurrency.cells[{index}]"
        cell = _fields(raw_cell, {
            "case_id", "concurrency", "boundary_dimension", "root_task_sha256",
            "family_task_sha256", "status", "initial", "before_boundary", "at_boundary",
            "after_rejected_overflow", "prefix", "contention", "overflow", "checks",
        }, label, {"error"})
        _require(isinstance(cell["case_id"], str) and cell["case_id"] not in by_id,
                 f"{label} has a missing/duplicate case ID")
        _require("error" not in cell or cell["error"] == "",
                 f"{label} carries an execution error")
        by_id[cell["case_id"]] = cell
    _require(set(by_id) == {case["id"] for case in config["cases"]},
             "concurrency report cases differ from config")
    for contract in config["cases"]:
        cell = by_id[contract["id"]]
        label = "concurrency case " + contract["id"]
        level, dimension = contract["concurrency"], contract["boundary_dimension"]
        maximum_width = max(maximum_width, level)
        _require(cell["status"] == "measured" and
                 _integer(cell["concurrency"], label + ".concurrency") == level and
                 cell["boundary_dimension"] == dimension,
                 f"{label} identity/status differs from config")
        _require(cell["root_task_sha256"] == _sha(contract["root_task_id"].encode()),
                 f"{label} root hash differs")
        family = sorted(_sha(one.encode()) for one in [contract["prefix_task_id"],
            contract["overflow_task_id"], *contract["contender_task_ids"]])
        _require(cell["family_task_sha256"] == family, f"{label} family task hashes differ")
        initial = _root_head(cell["initial"], label + ".initial")
        before = _root_head(cell["before_boundary"], label + ".before")
        at = _root_head(cell["at_boundary"], label + ".at")
        after = _root_head(cell["after_rejected_overflow"], label + ".after")
        zero = {"release": 0, "influence": 0, "outcome": 0}
        _require(initial["epoch"] == 0 and initial["used"] == zero and
                 initial["limits"] == contract["at_budget"] and len(initial) == 3,
                 f"{label} does not start at a fresh exact Catalog head")
        _require(before["epoch"] == 1 and before["used"] == contract["before_used"] and
                 before["limits"] == contract["at_budget"] and
                 before["used"][dimension] + 1 == before["limits"][dimension],
                 f"{label} did not establish exact B-1")
        _require(at["epoch"] == 2 and at["used"] == contract["at_budget"] and
                 at["limits"] == contract["at_budget"] and after == at,
                 f"{label} did not atomically commit B or B+1 mutated the head")
        digest_fields = {"release_set_sha256", "influence_set_sha256", "outcome_set_sha256"}
        _require(digest_fields <= set(before) and digest_fields <= set(at),
                 f"{label} nonempty root heads omit exact set commitments")
        prefix = _fields(cell["prefix"], {
            "status", "latency_ms", "observation_sha256", "actual", "charged", "root_epoch",
            "result_sha256",
        }, label + ".prefix")
        prefix_actual = _exposure_counts(prefix["actual"], label + ".prefix.actual")
        prefix_charged = _exposure_counts(prefix["charged"], label + ".prefix.charged")
        _require(prefix["status"] == "measured" and _number(prefix["latency_ms"], label + ".prefix latency", minimum=0) > 0 and
                 _digest(prefix["observation_sha256"], label + ".prefix observation") and
                 prefix_actual == contract["before_used"] and prefix_charged == contract["before_used"] and
                 _integer(prefix["root_epoch"], label + ".prefix root epoch") == before["epoch"] and
                 _digest(prefix["result_sha256"], label + ".prefix result"),
                 f"{label} prefix evidence is incoherent")
        contention = _fields(cell["contention"], {
            "status", "root_lock_waiters_observed",
            "successful_requests", "failed_requests", "charged_winners",
            "zero_novelty_settlements", "total_charged", "root_epochs", "observation_sha256",
            "result_sha256", "client_latency_ms",
        }, label + ".contention")
        expected_delta = _count_subtract(contract["at_budget"], contract["before_used"])
        contention_counts = {
            name: _integer(contention[name], label + ".contention." + name, minimum=0)
            for name in ("root_lock_waiters_observed", "successful_requests", "failed_requests",
                         "charged_winners", "zero_novelty_settlements")
        }
        total_charged = _exposure_counts(contention["total_charged"],
                                         label + ".contention.total_charged")
        _require(contention["status"] == "measured" and
                 contention_counts["successful_requests"] == level and
                 contention_counts["failed_requests"] == 0 and
                 contention_counts["charged_winners"] == 1 and
                 contention_counts["zero_novelty_settlements"] == level - 1 and
                 total_charged == expected_delta and contention_counts["root_lock_waiters_observed"] >= level,
                 f"{label} does not prove one winner plus zero-novelty settlements")
        total_zero_novelty += contention_counts["zero_novelty_settlements"]
        total_root_lock_waiters += contention_counts["root_lock_waiters_observed"]
        epochs = _list(contention["root_epochs"], label + ".root_epochs", length=level)
        observations = _list(contention["observation_sha256"], label + ".observations", length=level)
        results = _list(contention["result_sha256"], label + ".results", length=level)
        latencies = _list(contention["client_latency_ms"], label + ".latencies", length=level)
        _require(all(_integer(epoch, label + ".root epoch") == at["epoch"] for epoch in epochs) and
                 all(isinstance(one, str) and HEX64.fullmatch(one) for one in observations + results) and
                 len(set(observations)) == 1 and len(set(results)) == 1 and
                 all(_number(one, label + ".latency", minimum=0) > 0 for one in latencies),
                 f"{label} contender identities/epochs/latencies differ")
        overflow = _fields(cell["overflow"], {
            "status", "expected_error_code", "observed_error_code", "latency_ms", "query_status",
            "exposure_reservation_status", "encrypted_results", "encrypted_result_chunks",
            "materializations", "query_observations", "root_observations", "terminal_success_audits",
            "terminal_failure_audits", "receipts", "content_before", "content_after",
        }, label + ".overflow", {"query_result_sha256"})
        content_before = _content_counts(overflow["content_before"], label + ".content_before")
        content_after = _content_counts(overflow["content_after"], label + ".content_after")
        overflow_counts = {
            name: _integer(overflow[name], label + ".overflow." + name, minimum=0)
            for name in ("encrypted_results", "encrypted_result_chunks", "materializations", "query_observations",
                         "root_observations", "terminal_success_audits",
                         "terminal_failure_audits", "receipts")
        }
        _require(overflow["status"] == "rejected" and
                 overflow["expected_error_code"] == overflow["observed_error_code"] == "EXPOSURE_BUDGET_EXHAUSTED" and
                 _number(overflow["latency_ms"], label + ".overflow latency", minimum=0) > 0 and
                 overflow["query_status"] == "FAILED" and
                 overflow["exposure_reservation_status"] == "RELEASED" and
                 not overflow.get("query_result_sha256", "") and
                 all(overflow_counts[name] == 0 for name in ("encrypted_results", "encrypted_result_chunks", "materializations",
                    "query_observations", "root_observations", "terminal_success_audits")) and
                 overflow_counts["terminal_failure_audits"] == 1 and overflow_counts["receipts"] == 1 and
                 content_before == content_after,
                 f"{label} B+1 failure left partial state or lacks terminal failure evidence")
        checks = _fields(cell["checks"], {
            "shared_root_family", "fresh_root", "b_minus_one_committed", "b_committed",
            "three_dimensional_atomic", "root_lock_queue_observed", "overflow_rejected",
            "failure_left_no_partial_commit",
        }, label + ".checks")
        _require(all(one is True for one in checks.values()), f"{label} has a failed Boolean check")

    expected_gate_ids = {"source_and_config_binding", "concurrency_widths", "boundary_dimensions",
                         "all_root_lock_queues"}
    for contract in config["cases"]:
        prefix = "case_" + _safe_id(contract["id"]) + "_"
        expected_gate_ids.update(prefix + suffix for suffix in (
            "shared_root", "fresh_root", "b_minus_one", "b", "three_dimensional_atomicity",
            "root_lock_queue", "b_plus_one", "failure_atomicity"))
    gates = _list(report["gates"], "concurrency.gates", length=len(expected_gate_ids))
    seen: set[str] = set()
    for index, raw_gate in enumerate(gates):
        gate = _fields(raw_gate, {"id", "requirement", "status"}, f"concurrency.gates[{index}]",
                       {"evidence", "reason"})
        _require(isinstance(gate["id"], str) and gate["id"] in expected_gate_ids and
                 gate["id"] not in seen and gate["status"] == "pass" and
                 isinstance(gate["requirement"], str) and gate["requirement"] and not gate.get("reason"),
                 f"concurrency gate {gate.get('id')} is invalid/duplicate/not passing")
        seen.add(gate["id"])
    _require(seen == expected_gate_ids, "concurrency gate set is incomplete")
    return {
        "report": report,
        "case_count": len(cells),
        "maximum_concurrency": maximum_width,
        "concurrency_levels": expected_levels,
        "gateway_count": summary["contender_gateway_count"],
        "total_zero_novelty_settlements": total_zero_novelty,
        "total_root_lock_waiters": total_root_lock_waiters,
    }


def _maximum_point_identity(results: dict[str, Any]) -> dict[str, str]:
    samples = results.get("samples")
    _require(isinstance(samples, list), "base V4 results omit samples")
    selected: dict[str, str] | None = None
    count = 0
    for sample in samples:
        if not isinstance(sample, dict) or sample.get("phase") != "novel" or sample.get("status") != "measured":
            continue
        exposure = sample.get("exposure")
        if not isinstance(exposure, dict) or (
            exposure.get("actual_release_facts"), exposure.get("actual_influence_facts"),
            exposure.get("actual_outcome_facts")) != MAX_POINT:
            continue
        _require(exposure.get("profile_version") == "taskgate-exposure-v4",
                 "base maximum-point sample is not V4")
        identity = {
            "sha256": _digest(exposure.get("observation_sha256"), "base observation"),
            "dictionary_set_sha256": _digest(exposure.get("dictionary_set_digest"), "base dictionary set"),
            "release_set_sha256": _digest(exposure.get("release_set_sha256"), "base release set"),
            "influence_set_sha256": _digest(exposure.get("influence_set_sha256"), "base influence set"),
            "outcome_set_sha256": _digest(exposure.get("outcome_set_sha256"), "base outcome set"),
        }
        _integer(exposure.get("actual_release_facts"), "base actual release facts", minimum=0)
        _integer(exposure.get("actual_influence_facts"), "base actual influence facts", minimum=0)
        _integer(exposure.get("actual_outcome_facts"), "base actual outcome facts", minimum=0)
        if selected is None:
            selected = identity
        else:
            _require(selected == identity, "base maximum-point observations disagree")
        count += 1
    _require(selected is not None and count > 0, "base results contain no measured maximum-point novel sample")
    return selected


def _validate_oracle_report(value: dict[str, Any], results: dict[str, Any],
                            results_sha256: str, root: Path,
                            historical_source: dict[str, Any] | None = None
                            ) -> dict[str, Any]:
    report = _fields(value, {
        "schema_version", "oracle_id", "status", "started_at", "finished_at", "provenance",
        "independence_boundary", "observation", "fact_checks", "witness_checks", "resources", "gates",
    }, "million-Fact oracle report")
    _require(report["schema_version"] == "taskgate-v4-million-fact-oracle-v1" and
             report["oracle_id"] == "taskgate-independent-external-merge-oracle-v1" and
             report["status"] == "pass", "oracle identity/status is invalid")
    started, finished = _timestamp(report["started_at"], "oracle.started_at"), _timestamp(
        report["finished_at"], "oracle.finished_at")
    _require(finished >= started, "oracle timestamps are reversed")
    provenance = _fields(report["provenance"], {
        "results_sha256", "oracle_package_sha256", "repository_source_scope_sha256",
        "repository_source_scope_files", "executable_sha256", "cold_artifacts",
    }, "oracle.provenance")
    if historical_source is None:
        repository_sha, repository_paths = _oracle_repository_source_scope(root)
        repository_files = len(repository_paths)
        package_sha = _oracle_package_digest(root)
    else:
        repository_sha = _digest(
            historical_source["oracle_repository_sha256"],
            "historical oracle repository source")
        repository_files = _integer(
            historical_source["oracle_repository_files"],
            "historical oracle repository source files", minimum=1)
        package_sha = _digest(
            historical_source["oracle_package_sha256"],
            "historical oracle package source")
    _require(provenance["results_sha256"] == results_sha256 and
             provenance["oracle_package_sha256"] == package_sha and
             provenance["repository_source_scope_sha256"] == repository_sha and
             _integer(provenance["repository_source_scope_files"],
                      "oracle repository source file count", minimum=1) == repository_files and
             isinstance(provenance["executable_sha256"], str) and
             HEX64.fullmatch(provenance["executable_sha256"]) is not None,
             "oracle evidence/source/executable binding is stale")
    artifacts = _list(provenance["cold_artifacts"], "oracle cold artifacts", length=2)
    artifact_names: set[str] = set()
    for index, raw_artifact in enumerate(artifacts):
        artifact = _fields(raw_artifact, {
            "publication_name", "dictionary_sha256", "manifest_sha256", "artifact_sha256", "bytes",
        }, f"oracle cold_artifacts[{index}]")
        _require(isinstance(artifact["publication_name"], str) and
                 artifact["publication_name"] in {"scale-orders-v4-narrow-1", "scale-lineitem-v4-narrow-1"} and
                 artifact["publication_name"] not in artifact_names,
                 "oracle cold artifact publication is unexpected/duplicate")
        artifact_names.add(artifact["publication_name"])
        for name in ("dictionary_sha256", "manifest_sha256", "artifact_sha256"):
            _digest(artifact[name], "oracle artifact " + name)
        _integer(artifact["bytes"], "oracle artifact bytes", minimum=1)
    _require(artifact_names == {"scale-orders-v4-narrow-1", "scale-lineitem-v4-narrow-1"},
             "oracle does not bind both frozen publications")
    boundary = _fields(report["independence_boundary"], {
        "expected_source", "actual_source", "algorithm", "independence_scope", "evidence_validation",
        "v4_bitmap_derivation_hot_path_calls",
    }, "oracle.independence_boundary")
    _require(boundary["expected_source"] ==
             "independent row-wise reconstruction from frozen reporting.scale_orders and reporting.scale_lineitem" and
             boundary["actual_source"] ==
             "committed Control-PG containers plus independently streamed COLD FactIDs" and
             boundary["algorithm"] ==
             "bounded external merge sort by full FactHash; exact canonical-payload and witness-multiplicity comparison" and
             boundary["independence_scope"] ==
             "derivation-independent; shares the versioned canonical FactID specification and encoder with TaskGate" and
             boundary["evidence_validation"] ==
             "strict duplicate-key/trailing JSON rejection plus full-file SHA-256 binding of source-controlled V4 results" and
             _integer(boundary["v4_bitmap_derivation_hot_path_calls"],
                      "oracle V4 hot-path calls", minimum=0) == 0,
             "oracle independence boundary is incomplete or overstated")
    expected_observation = _maximum_point_identity(results)
    observation = _fields(report["observation"], {
        "sha256", "dictionary_set_sha256", "release_set_sha256", "influence_set_sha256",
        "outcome_set_sha256", "recomputed_sha256", "normal_form_sha256",
    }, "oracle.observation")
    for name, digest in observation.items():
        _digest(digest, "oracle observation " + name)
    _require(all(observation[name] == expected_observation[name] for name in expected_observation) and
             observation["recomputed_sha256"] == observation["sha256"],
             "oracle observation/effect identity differs from base results or recomputation")
    facts = _fields(report["fact_checks"], {
        "expected_release", "actual_release", "matched_release", "expected_influence", "actual_influence",
        "matched_influence", "expected_outcome", "actual_outcome", "matched_outcome", "fact_hash_matches",
        "canonical_payload_matches", "total_compared", "hash_mismatches",
        "canonical_payload_mismatches", "missing_facts", "extra_facts", "influence_chunk_sha256",
    }, "oracle.fact_checks")
    fact_numbers = {
        name: _integer(facts[name], "oracle.fact_checks." + name, minimum=0)
        for name in set(facts) - {"influence_chunk_sha256"}
    }
    _require((fact_numbers["expected_release"], fact_numbers["actual_release"],
              fact_numbers["matched_release"]) == (12, 12, 12) and
             (fact_numbers["expected_influence"], fact_numbers["actual_influence"],
              fact_numbers["matched_influence"]) ==
             (1_035_000, 1_035_000, 1_035_000) and
             (fact_numbers["expected_outcome"], fact_numbers["actual_outcome"],
              fact_numbers["matched_outcome"]) == (1, 1, 1) and
             fact_numbers["fact_hash_matches"] == fact_numbers["canonical_payload_matches"] ==
             fact_numbers["total_compared"] == TOTAL_MAX_POINT and
             all(fact_numbers[name] == 0 for name in ("hash_mismatches", "canonical_payload_mismatches",
                                                      "missing_facts", "extra_facts")),
             "oracle FactHash/payload exact matching is incomplete")
    chunks = _list(facts["influence_chunk_sha256"], "oracle influence chunks", length=16)
    _require(all(isinstance(one, str) and HEX64.fullmatch(one) for one in chunks) and
             len(set(chunks)) == 16,
             "oracle influence chunk commitments are malformed/duplicate")
    witnesses = _fields(report["witness_checks"], {
        "derived_facts", "matched_commitments", "commitment_mismatches", "expected_witness_items",
        "expected_total_multiplicity", "commitment_set_sha256", "multiplicity_stream_sha256",
    }, "oracle.witness_checks")
    witness_numbers = {
        name: _integer(witnesses[name], "oracle.witness_checks." + name, minimum=0)
        for name in ("derived_facts", "matched_commitments", "commitment_mismatches",
                     "expected_witness_items", "expected_total_multiplicity")
    }
    _require(witness_numbers["derived_facts"] == witness_numbers["matched_commitments"] == 12 and
             witness_numbers["commitment_mismatches"] == 0 and
             witness_numbers["expected_witness_items"] == 1_035_000 and
             witness_numbers["expected_total_multiplicity"] == 1_800_000,
             "oracle witness commitment/multiplicity evidence is incomplete")
    _digest(witnesses["commitment_set_sha256"], "oracle commitment set")
    _digest(witnesses["multiplicity_stream_sha256"], "oracle multiplicity stream")
    resources = _fields(report["resources"], {
        "sort_memory_limit_bytes", "theoretical_buffer_bound_bytes", "spool_bytes", "sort_runs",
        "sort_run_sha256", "maximum_resident_records", "peak_rss_bytes", "business_rows",
        "cold_facts_scanned",
    }, "oracle.resources")
    sort_memory = _integer(resources["sort_memory_limit_bytes"], "oracle sort memory", minimum=1 << 20)
    _require(_integer(resources["theoretical_buffer_bound_bytes"],
                      "oracle theoretical buffer bound", minimum=1) == sort_memory * 14 and
             _integer(resources["spool_bytes"], "oracle spool bytes", minimum=1) > 0 and
             _integer(resources["sort_runs"], "oracle sort runs", minimum=1) > 0 and
             _integer(resources["maximum_resident_records"], "oracle maximum resident records", minimum=1) > 0 and
             _integer(resources["peak_rss_bytes"], "oracle peak RSS", minimum=1) > 0 and
             _integer(resources["business_rows"], "oracle business rows", minimum=1) == 225_000 and
             _integer(resources["cold_facts_scanned"], "oracle cold facts scanned", minimum=1) == 2_600_000,
             "oracle bounded external-merge resource evidence is incoherent")
    sort_runs = _integer(resources["sort_runs"], "oracle sort runs", minimum=1)
    runs = _list(resources["sort_run_sha256"], "oracle sort run hashes", length=sort_runs)
    _require(all(isinstance(one, str) and HEX64.fullmatch(one) for one in runs),
             "oracle sort-run hash is malformed")
    gates = _list(report["gates"], "oracle.gates", length=len(EXPECTED_ORACLE_GATES))
    seen: set[str] = set()
    for index, raw_gate in enumerate(gates):
        gate = _fields(raw_gate, {"id", "requirement", "status", "evidence"},
                       f"oracle.gates[{index}]", {"reason"})
        _require(isinstance(gate["id"], str) and gate["id"] in EXPECTED_ORACLE_GATES and
                 gate["id"] not in seen and
                 gate["status"] == "pass" and isinstance(gate["requirement"], str) and gate["requirement"] and
                 not gate.get("reason"), f"oracle gate {gate.get('id')} is invalid/not passing")
        seen.add(gate["id"])
    _require(seen == EXPECTED_ORACLE_GATES, "oracle gate set is incomplete")
    return {
        "report": report,
        "total_compared": TOTAL_MAX_POINT,
        "total_mismatches": sum(fact_numbers[name] for name in (
            "hash_mismatches", "canonical_payload_mismatches", "missing_facts", "extra_facts")),
        "witnesses": witness_numbers["matched_commitments"],
        "witness_multiplicity": witness_numbers["expected_total_multiplicity"],
        "duration_seconds": (finished - started).total_seconds(),
        "peak_rss_bytes": resources["peak_rss_bytes"],
        "spool_bytes": resources["spool_bytes"],
        "cold_facts_scanned": resources["cold_facts_scanned"],
        "sort_runs": sort_runs,
    }


def validate_v4_supplemental_evidence(repository_root: Path | str,
                                      evidence_dir: Path | str | None = None) -> dict[str, Any]:
    """Validate and return recomputed statistics for all supplemental axes.

    ``evidence_dir`` changes only where the six local evidence files are read;
    manifest artifact names remain canonical repository-relative names.  The
    retained base V4 results are always resolved from ``repository_root``.
    The canonical source-controlled pack is bound to its immutable historical
    source archive; an alternate candidate directory remains bound to the
    current repository source so it can be checked before archival.
    """
    root = Path(repository_root)
    try:
        root_info = os.lstat(root)
    except OSError as error:
        raise ValueError(f"V4 supplemental evidence: cannot stat repository root: {error}") from error
    _require(stat.S_ISDIR(root_info.st_mode) and not stat.S_ISLNK(root_info.st_mode),
             "repository root is missing or a symlink")
    historical_source = None
    if evidence_dir is None:
        provenance_raw = _read_regular(root / HISTORICAL_SOURCE_REL, 1 << 20)
        archive_raw = _read_regular(root / HISTORICAL_ARCHIVE_REL, 4 << 20)
        historical_source = _historical_source_snapshot(provenance_raw, archive_raw)
    manifest, retained = _load_manifest(root, evidence_dir, historical_source)
    local_prefix = EVIDENCE_REL.as_posix() + "/"
    artifact = lambda name: retained[local_prefix + name]

    results_raw = retained[V4_RESULTS_REL.as_posix()]
    results = _decode_json(results_raw, "retained base V4 results")
    results_sha = _sha(results_raw)
    environment = _validate_environment(_decode_json(artifact("environment.json"), "environment"),
                                        results_sha)
    distribution = _validate_distribution_report(
        _decode_json(artifact("distribution.json"), "distribution report"))
    concurrency_config_raw = artifact("concurrency-config.json")
    concurrency_config = _validate_concurrency_config(
        _decode_json(concurrency_config_raw, "concurrency config"))
    concurrency = _validate_concurrency_report(
        _decode_json(artifact("concurrency.json"), "concurrency report"),
        concurrency_config, concurrency_config_raw, root,
        None if historical_source is None else historical_source["concurrency_source_sha256"])
    oracle = _validate_oracle_report(
        _decode_json(artifact("million-oracle.json"), "million-Fact oracle report"),
        results, results_sha, root, historical_source)
    if historical_source is None:
        source_sha, source_files, current_paths = _generalized_source_scope(root)
        source_paths = [path.relative_to(root).as_posix() for path in current_paths]
        current_relation = {
            "status": "match", "current_source_sha256": source_sha,
            "current_source_files": source_files, "historical_source_sha256": None,
            "matches_historical": True,
        }
    else:
        source_sha = historical_source["source_scope_sha256"]
        source_files = historical_source["source_scope_files"]
        source_paths = historical_source["source_scope_paths"]
        current_relation = _current_source_relation(root, source_sha)
    return {
        "manifest": manifest,
        "historical_source": historical_source,
        "current_source_relation": current_relation,
        "environment": environment,
        "distribution": distribution,
        "concurrency": concurrency,
        "oracle": oracle,
        "stats": {
            "source_scope_sha256": source_sha,
            "source_scope_files": source_files,
            "source_scope_paths": source_paths,
            "current_source_relation": current_relation["status"],
            "current_source_matches_historical": current_relation["matches_historical"],
            "distribution_cells": 12,
            "distribution_runs_per_cell": distribution["runs_per_cell"],
            "distribution_peak_heap_bytes": distribution["max_peak_heap_bytes"],
            "concurrency_cells": concurrency["case_count"],
            "maximum_concurrency": concurrency["maximum_concurrency"],
            "oracle_total_compared": oracle["total_compared"],
            "oracle_peak_rss_bytes": oracle["peak_rss_bytes"],
        },
    }


def validate_v4_supplemental(repository_root: Path | str,
                             evidence_dir: Path | str | None = None) -> dict[str, Any]:
    return validate_v4_supplemental_evidence(repository_root, evidence_dir)


__all__ = ["validate_v4_supplemental", "validate_v4_supplemental_evidence"]
