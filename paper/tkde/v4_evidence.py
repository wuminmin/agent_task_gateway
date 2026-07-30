#!/usr/bin/env python3
"""Fail-closed validation for the source-controlled TaskGate V4 evidence.

The module deliberately has no dependency on the paper generator.  Its public
entry point, :func:`validate_v4_evidence`, validates the retained artifacts and
returns only statistics recomputed from raw samples.
"""

from __future__ import annotations

import hashlib
import io
import json
import math
import os
import re
import stat
import tarfile
from pathlib import Path, PurePosixPath
from typing import Any, Iterable


EVIDENCE_REL = Path("evaluation/v4-acceptance/evidence")
BASELINE_REL = Path("evaluation/exposure-performance/results.json")
RUNNER_REL = Path("evaluation/cmd/v4-acceptance/runner.go")
HISTORICAL_SOURCE_REL = EVIDENCE_REL / "historical-source.json"
HISTORICAL_ARCHIVE_REL = EVIDENCE_REL / "historical-source-e8e751c.tar.gz"

EXPECTED_CAMPAIGN = "taskgate-v4-full-20260730t070232z"
EXPECTED_HISTORICAL_COMMIT = "e8e751c666b85b436e7fa2960be23b18f3d2e515"
EXPECTED_HISTORICAL_TREE = "49a0e587d2e8f429cc931fcf8046c7616a93d5dc"
EXPECTED_HISTORICAL_SOURCE_SHA256 = "20ae76efb71df276774becc066e084061bd181b408e75109668e4256f29c613c"
EXPECTED_HISTORICAL_PATHS_SHA256 = "b70b5a716821f648e5061c3ea1d5964cc2d2c76615fe5d4a679cb4883f755ba7"
EXPECTED_HISTORICAL_ARCHIVE_SHA256 = "53bc8eb3e70ec9fedfdad1cc1f6d5c6f3a36e90e62a1b031fad303a7261b8466"
EXPECTED_HISTORICAL_SOURCE_FILES = 187
EXPECTED_HISTORICAL_MTIME = 1_785_402_674
SOURCE_DIGEST_ALGORITHM = "SHA-256 over sorted UTF-8 path, NUL, exact bytes, NUL frames"
ARCHIVE_GENERATION = (
    "git -c tar.umask=0022 archive --format=tar <commit> -- <sorted source paths> | gzip -n -9"
)
SOURCE_SELECTION = (
    "sourceDigestRoots/sourceDigestFiles parsed from archived "
    "evaluation/cmd/v4-acceptance/runner.go"
)
EXPECTED_LOCAL_FILES = {
    "README.md",
    "manifest.json",
    "results.json",
    "full-config.json",
    "environment.json",
    "activation-verification-receipt.json",
    "preflight-artifact-verification-receipt.json",
    "small-query-candidate.json",
    "small-query-results.json",
    "small-query-samples.jsonl",
    "small-query-docker-stats.jsonl",
    "historical-source.json",
    "historical-source-e8e751c.tar.gz",
}
EXPECTED_ARTIFACTS = {
    "evaluation/v4-acceptance/evidence/results.json",
    "evaluation/v4-acceptance/evidence/full-config.json",
    "evaluation/v4-acceptance/evidence/environment.json",
    "evaluation/v4-acceptance/evidence/activation-verification-receipt.json",
    "evaluation/v4-acceptance/evidence/preflight-artifact-verification-receipt.json",
    "evaluation/v4-acceptance/evidence/small-query-candidate.json",
    "evaluation/v4-acceptance/evidence/small-query-results.json",
    "evaluation/v4-acceptance/evidence/small-query-samples.jsonl",
    "evaluation/v4-acceptance/evidence/small-query-docker-stats.jsonl",
    "evaluation/v4-acceptance/evidence/historical-source.json",
    "evaluation/v4-acceptance/evidence/historical-source-e8e751c.tar.gz",
    "evaluation/exposure-performance/results.json",
}
EXPECTED_GATES = {
    "evidence_provenance", "fixed_environment_manifest", "execution_integrity",
    "required_observer", "overlap_0", "overlap_50", "overlap_90", "overlap_100",
    "shape_scan", "shape_join_group", "shape_union", "shape_page", "novel_latency",
    "semantic_replay_latency", "semantic_replay_gateway_sql_components",
    "semantic_replay_no_business_sql", "gateway_cgroup_peak_memory",
    "network_measurement", "wal_measurement", "index_build_time", "index_builder_rss",
    "artifact_total", "artifact_hot", "activation_strict_verification",
    "activation_time", "storage_measurement", "bitmap_derivation_end_to_end",
    "ordinal_stream_end_to_end", "settlement_measurement", "small_query_regression",
}
HEX64 = re.compile(r"^[0-9a-f]{64}$")
TASK_ID = re.compile(r"^task_[0-9a-f]{32}$")
RECEIPT_DOMAIN = b"taskgate/snapshot-verification-receipt/v1\x00"
MAX_POINT = (12, 1_035_000, 1)


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError("V4 evidence: " + message)


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
            raw.decode("utf-8"), object_pairs_hook=_object_no_duplicates,
            parse_constant=_reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        raise ValueError(f"V4 evidence: invalid {label}: {error}") from error
    _require(isinstance(value, dict), f"{label} must contain one JSON object")
    return value


def _read_regular(path: Path, maximum: int) -> bytes:
    try:
        before = os.lstat(path)
    except OSError as error:
        raise ValueError(f"V4 evidence: cannot stat {path}: {error}") from error
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
        raise ValueError(f"V4 evidence: cannot read {path}: {error}") from error
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


def _number(value: Any, label: str, *, minimum: float | None = None) -> float:
    _require(isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(float(value)),
             f"{label} is not finite numeric evidence")
    result = float(value)
    if minimum is not None:
        _require(result >= minimum, f"{label} is below {minimum}")
    return result


def _integer(value: Any, label: str, *, minimum: int | None = None) -> int:
    _require(isinstance(value, int) and not isinstance(value, bool), f"{label} is not an integer")
    if minimum is not None:
        _require(value >= minimum, f"{label} is below {minimum}")
    return value


def _close(actual: Any, expected: Any, label: str) -> None:
    left = _number(actual, label)
    right = _number(expected, label + " expected")
    _require(math.isclose(left, right, rel_tol=1e-12, abs_tol=1e-9),
             f"{label} differs: {left} != {right}")


def _percentile(values: Iterable[float], probability: float) -> float:
    ordered = sorted(float(value) for value in values)
    _require(bool(ordered), "cannot compute a percentile of no samples")
    position = (len(ordered) - 1) * probability
    lower, upper = math.floor(position), math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def _distribution(values: Iterable[float]) -> dict[str, float | int]:
    ordered = sorted(float(value) for value in values)
    _require(bool(ordered) and all(math.isfinite(value) and value >= 0 for value in ordered),
             "distribution contains no samples or a negative/non-finite value")
    return {
        "count": len(ordered), "min": ordered[0], "p50": _percentile(ordered, 0.50),
        "p95": _percentile(ordered, 0.95), "p99": _percentile(ordered, 0.99),
        "max": ordered[-1], "mean": sum(ordered) / len(ordered),
    }


def _validate_distribution(reported: Any, expected: dict[str, float | int], label: str) -> None:
    _require(isinstance(reported, dict) and set(reported) == set(expected),
             f"{label} distribution has unexpected fields")
    _require(reported["count"] == expected["count"], f"{label} count differs")
    for key in ("min", "p50", "p95", "p99", "max", "mean"):
        _close(reported[key], expected[key], f"{label}.{key}")


def _load_manifest(root: Path, evidence_dir: Path | None = None) -> tuple[dict[str, Any], dict[str, bytes]]:
    directory = (root / EVIDENCE_REL) if evidence_dir is None else evidence_dir
    try:
        directory_info = os.lstat(directory)
    except OSError as error:
        raise ValueError(f"V4 evidence: cannot stat evidence directory: {error}") from error
    _require(stat.S_ISDIR(directory_info.st_mode) and not stat.S_ISLNK(directory_info.st_mode),
             "evidence directory is missing or a symlink")
    actual_names = set()
    with os.scandir(directory) as entries:
        for entry in entries:
            _require(entry.is_file(follow_symlinks=False),
                     f"unexpected non-file evidence entry {entry.name}")
            actual_names.add(entry.name)
    _require(actual_names == EXPECTED_LOCAL_FILES,
             f"evidence directory set differs: {sorted(actual_names ^ EXPECTED_LOCAL_FILES)}")

    manifest = _decode_json(_read_regular(directory / "manifest.json", 1 << 20), "V4 manifest")
    _require(set(manifest) == {"schema_version", "campaign_id", "status", "acceptance",
                               "acceptance_source_sha256", "artifacts"},
             "manifest fields are not exact")
    _require(manifest["schema_version"] == 1 and manifest["campaign_id"] == EXPECTED_CAMPAIGN and
             manifest["status"] == "complete_measured_campaign" and manifest["acceptance"] == "pass",
             "manifest identity/status is invalid")
    _digest(manifest["acceptance_source_sha256"], "manifest acceptance source")
    artifacts = manifest["artifacts"]
    _require(isinstance(artifacts, dict) and set(artifacts) == EXPECTED_ARTIFACTS,
             "manifest artifact set is not exact")

    maxima = {
        "results.json": 8 << 20, "full-config.json": 1 << 20, "environment.json": 1 << 20,
        "activation-verification-receipt.json": 4 << 20,
        "preflight-artifact-verification-receipt.json": 4 << 20,
        "small-query-candidate.json": 4 << 20, "small-query-results.json": 4 << 20,
        "small-query-samples.jsonl": 16 << 20, "small-query-docker-stats.jsonl": 4 << 20,
        "historical-source.json": 1 << 20, "historical-source-e8e751c.tar.gz": 4 << 20,
        "evaluation/exposure-performance/results.json": 4 << 20,
    }
    retained: dict[str, bytes] = {}
    for relative in sorted(EXPECTED_ARTIFACTS):
        if relative.startswith(EVIDENCE_REL.as_posix() + "/"):
            path = directory / Path(relative).name
        else:
            path = root / relative
        maximum = maxima.get(path.name, maxima.get(relative, 4 << 20))
        raw = _read_regular(path, maximum)
        _require(_sha(raw) == _digest(artifacts[relative], f"manifest digest for {relative}"),
                 f"artifact digest differs for {relative}")
        retained[relative] = raw
    return manifest, retained


def _parse_go_string_slice(source: str, name: str) -> list[str]:
    matches = list(re.finditer(rf"(?ms)^var\s+{re.escape(name)}\s*=\s*\[\]string\s*\{{(.*?)^\}}", source))
    _require(len(matches) == 1, f"runner.go must declare {name} exactly once")
    result: list[str] = []
    for line in matches[0].group(1).splitlines():
        if not line.strip():
            continue
        match = re.fullmatch(r'\s*("(?:[^"\\]|\\.)*")\s*,\s*', line)
        _require(match is not None, f"cannot parse {name} declaration line {line!r}")
        try:
            value = json.loads(match.group(1))
        except json.JSONDecodeError as error:
            raise ValueError(f"V4 evidence: invalid Go string in {name}: {error}") from error
        parts = PurePosixPath(value).parts if isinstance(value, str) else ()
        _require(isinstance(value, str) and value and "\\" not in value and "\0" not in value and
                 not value.startswith("/") and all(part not in {"", ".", ".."} for part in parts),
                 f"unsafe path in {name}")
        result.append(value)
    _require(result and len(result) == len(set(result)), f"{name} is empty or repeats a path")
    return result


def _current_source_digest(root: Path) -> str:
    runner_raw = _read_regular(root / RUNNER_REL, 2 << 20)
    try:
        runner = runner_raw.decode("utf-8")
    except UnicodeDecodeError as error:
        raise ValueError("V4 evidence: runner.go is not UTF-8") from error
    roots = _parse_go_string_slice(runner, "sourceDigestRoots")
    explicit = _parse_go_string_slice(runner, "sourceDigestFiles")
    paths: list[Path] = []
    for relative in roots:
        directory = root / relative
        _require(directory.is_dir() and not directory.is_symlink(), f"source root {relative} is invalid")
        count = 0
        for walk_root, directories, files in os.walk(directory, followlinks=False):
            for dirname in directories:
                _require(not (Path(walk_root) / dirname).is_symlink(),
                         f"source root {relative} contains a directory symlink")
            for filename in files:
                path = Path(walk_root) / filename
                if path.suffix not in {".go", ".sql"}:
                    continue
                _require(not path.is_symlink() and path.is_file(), f"source artifact {path} is invalid")
                paths.append(path)
                count += 1
        _require(count > 0, f"source root {relative} contains no Go or SQL files")
    for relative in explicit:
        path = root / relative
        _require(path.is_file() and not path.is_symlink(), f"explicit source artifact {relative} is invalid")
        paths.append(path)
    _require(len(paths) == len(set(paths)), "runner source manifest resolves duplicate paths")
    checksum = hashlib.sha256()
    for path in sorted(paths, key=lambda item: item.relative_to(root).as_posix()):
        relative = path.relative_to(root).as_posix()
        checksum.update(relative.encode("utf-8"))
        checksum.update(b"\0")
        checksum.update(_read_regular(path, 32 << 20))
        checksum.update(b"\0")
    return checksum.hexdigest()


def _historical_source_snapshot(provenance_raw: bytes, archive_raw: bytes,
                                reported_source_sha256: str) -> dict[str, Any]:
    """Validate the immutable source snapshot that produced the V4 campaign.

    The archive is never extracted to disk.  It is read sequentially with
    strict compressed/decompressed/member bounds, and every member path and
    type is checked before its bytes are retained in the small in-memory source
    map.  The archived runner defines the source selection; fixed path and
    content digests prevent that definition from silently omitting a file.
    """

    provenance = _decode_json(provenance_raw, "historical source provenance")
    expected_fields = {
        "schema_version", "campaign_id", "git_commit", "git_tree", "archive",
        "archive_sha256", "archive_generation", "source_sha256", "source_file_count",
        "source_paths_sha256", "source_digest_algorithm", "source_selection",
    }
    _require(set(provenance) == expected_fields,
             "historical source provenance fields are not exact")
    _require(
        _integer(provenance["schema_version"], "historical source schema") == 1 and
        provenance["campaign_id"] == EXPECTED_CAMPAIGN and
        provenance["git_commit"] == EXPECTED_HISTORICAL_COMMIT and
        provenance["git_tree"] == EXPECTED_HISTORICAL_TREE and
        provenance["archive"] == HISTORICAL_ARCHIVE_REL.as_posix() and
        provenance["archive_generation"] == ARCHIVE_GENERATION and
        provenance["source_digest_algorithm"] == SOURCE_DIGEST_ALGORITHM and
        provenance["source_selection"] == SOURCE_SELECTION,
        "historical source identity/commit/algorithm is invalid",
    )
    _require(0 < len(archive_raw) <= 4 << 20, "historical source archive exceeds its compressed bound")
    _require(archive_raw[:10] == b"\x1f\x8b\x08\x00\x00\x00\x00\x00\x02\x03",
             "historical source archive lacks the deterministic gzip -n -9 header")
    archive_sha = _sha(archive_raw)
    _require(
        archive_sha == _digest(provenance["archive_sha256"], "historical source archive") ==
        EXPECTED_HISTORICAL_ARCHIVE_SHA256,
        "historical source archive digest is stale",
    )
    _require(
        _digest(provenance["source_sha256"], "historical source") ==
        reported_source_sha256 == EXPECTED_HISTORICAL_SOURCE_SHA256 and
        _integer(provenance["source_file_count"], "historical source file count", minimum=1) ==
        EXPECTED_HISTORICAL_SOURCE_FILES and
        _digest(provenance["source_paths_sha256"], "historical source paths") ==
        EXPECTED_HISTORICAL_PATHS_SHA256,
        "historical source provenance does not bind the measured source scope",
    )

    files: dict[str, bytes] = {}
    directories: set[str] = set()
    member_names: set[str] = set()
    member_count = 0
    total_bytes = 0
    pax_headers: dict[str, str] = {}
    try:
        with tarfile.open(fileobj=io.BytesIO(archive_raw), mode="r|gz") as archive:
            for member in archive:
                member_count += 1
                _require(member_count <= 512, "historical source archive has too many members")
                name = member.name
                path = PurePosixPath(name)
                _require(
                    isinstance(name, str) and name and name == path.as_posix() and
                    not path.is_absolute() and "\\" not in name and "\0" not in name and
                    all(part not in {"", ".", ".."} for part in path.parts) and
                    name not in member_names,
                    f"unsafe or duplicate historical source member {name!r}",
                )
                member_names.add(name)
                _require(
                    member.uid == 0 and member.gid == 0 and member.uname == "root" and
                    member.gname == "root" and member.mtime == EXPECTED_HISTORICAL_MTIME and
                    member.pax_headers == {"comment": EXPECTED_HISTORICAL_COMMIT} and
                    not member.linkname,
                    f"historical source member {name!r} metadata is not deterministic",
                )
                if member.isdir():
                    _require(member.size == 0 and member.mode == 0o755,
                             f"historical source directory {name!r} metadata is invalid")
                    directories.add(name)
                    continue
                _require(member.isreg() and member.mode in {0o644, 0o755},
                         f"historical source member {name!r} is not a normalized regular file")
                _require(0 < member.size <= 32 << 20,
                         f"historical source member {name!r} exceeds its bound")
                total_bytes += member.size
                _require(total_bytes <= 32 << 20,
                         "historical source archive exceeds its decompressed bound")
                extracted = archive.extractfile(member)
                _require(extracted is not None, f"cannot read historical source member {name!r}")
                raw = extracted.read(member.size + 1)
                _require(len(raw) == member.size,
                         f"historical source member {name!r} changed size while read")
                files[name] = raw
            pax_headers = dict(archive.pax_headers)
    except (tarfile.TarError, OSError, EOFError) as error:
        raise ValueError(f"V4 evidence: invalid historical source archive: {error}") from error
    _require(pax_headers == {"comment": EXPECTED_HISTORICAL_COMMIT},
             "historical source archive does not bind the exact Git commit")

    expected_directories: set[str] = set()
    for relative in files:
        parent = PurePosixPath(relative).parent
        while parent != PurePosixPath("."):
            expected_directories.add(parent.as_posix())
            parent = parent.parent
    _require(directories == expected_directories and member_names == directories | set(files),
             "historical source archive directory/member set is not exact")

    runner_name = RUNNER_REL.as_posix()
    _require(runner_name in files, "historical source archive omits its source-scope runner")
    try:
        runner = files[runner_name].decode("utf-8")
    except UnicodeDecodeError as error:
        raise ValueError("V4 evidence: archived acceptance runner is not UTF-8") from error
    roots = _parse_go_string_slice(runner, "sourceDigestRoots")
    explicit = _parse_go_string_slice(runner, "sourceDigestFiles")
    resolved: list[str] = []
    for root in roots:
        prefix = root.rstrip("/") + "/"
        selected = sorted(relative for relative in files if relative.startswith(prefix) and
                          PurePosixPath(relative).suffix in {".go", ".sql"})
        _require(bool(selected), f"archived source root {root} contains no Go or SQL files")
        resolved.extend(selected)
    resolved.extend(explicit)
    _require(len(resolved) == len(set(resolved)),
             "archived runner source scope resolves duplicate files")
    _require(set(files) == set(resolved),
             "historical source archive regular-file set differs from its archived runner")

    path_digest = hashlib.sha256()
    source_digest = hashlib.sha256()
    for relative in sorted(files):
        encoded = relative.encode("utf-8")
        path_digest.update(encoded)
        path_digest.update(b"\0")
        source_digest.update(encoded)
        source_digest.update(b"\0")
        source_digest.update(files[relative])
        source_digest.update(b"\0")
    _require(
        len(files) == EXPECTED_HISTORICAL_SOURCE_FILES and
        path_digest.hexdigest() == provenance["source_paths_sha256"] ==
        EXPECTED_HISTORICAL_PATHS_SHA256 and
        source_digest.hexdigest() == provenance["source_sha256"] ==
        reported_source_sha256 == EXPECTED_HISTORICAL_SOURCE_SHA256,
        "historical source path/content digest is not reproducible",
    )
    return {
        "mode": "historical_source_snapshot",
        "campaign_id": EXPECTED_CAMPAIGN,
        "git_commit": EXPECTED_HISTORICAL_COMMIT,
        "git_tree": EXPECTED_HISTORICAL_TREE,
        "archive_sha256": archive_sha,
        "source_sha256": source_digest.hexdigest(),
        "source_paths_sha256": path_digest.hexdigest(),
        "source_file_count": len(files),
        "archive_member_count": member_count,
        "archive_uncompressed_source_bytes": total_bytes,
    }


def _current_source_relation(root: Path, historical_source_sha256: str) -> dict[str, Any]:
    try:
        current = _current_source_digest(root)
    except ValueError as error:
        return {
            "status": "unavailable", "current_source_sha256": None,
            "historical_source_sha256": historical_source_sha256,
            "matches_historical": False, "reason": str(error),
        }
    matches = current == historical_source_sha256
    return {
        "status": "match" if matches else "diverged",
        "current_source_sha256": current,
        "historical_source_sha256": historical_source_sha256,
        "matches_historical": matches,
    }


def _canonical_receipt(raw: bytes, label: str) -> dict[str, Any]:
    receipt = _decode_json(raw, label)
    _require(set(receipt) == {"schema_version", "verified_at", "artifact_root", "publications",
                              "receipt_body_sha256"}, f"{label} fields are not exact")
    _require(receipt["schema_version"] == "taskgate-snapshot-verification-receipt-v1",
             f"{label} schema is invalid")
    canonical = (json.dumps(receipt, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8")
    _require(canonical == raw, f"{label} is not canonical JSON")
    body_digest = _digest(receipt["receipt_body_sha256"], f"{label} body digest")
    body = dict(receipt)
    body["receipt_body_sha256"] = ""
    encoded = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    _require(_sha(RECEIPT_DOMAIN + encoded) == body_digest, f"{label} body digest differs")
    publications = receipt["publications"]
    _require(isinstance(publications, list) and len(publications) == 2, f"{label} publication count differs")
    seen: set[str] = set()
    root_identity = receipt.get("artifact_root", {})
    _require(isinstance(root_identity, dict) and
             _integer(root_identity.get("device"), f"{label} root device", minimum=1) > 0 and
             _integer(root_identity.get("inode"), f"{label} root inode", minimum=1) > 0,
             f"{label} artifact-root identity is invalid")
    for publication in publications:
        _require(isinstance(publication, dict), f"{label} publication is not an object")
        name = publication.get("publication_name")
        _require(name in {"scale-orders-v4-narrow-1", "scale-lineitem-v4-narrow-1"} and name not in seen,
                 f"{label} publication identity is invalid")
        seen.add(name)
        _digest(publication.get("bundle_sha256"), f"{label} {name} bundle")
        directory_identity = publication.get("directory", {})
        _require(isinstance(directory_identity, dict) and
                 _integer(directory_identity.get("device"), f"{label} directory device", minimum=1) > 0 and
                 _integer(directory_identity.get("inode"), f"{label} directory inode", minimum=1) > 0,
                 f"{label} {name} directory identity is invalid")
        measurement = publication.get("measurement")
        _require(isinstance(measurement, dict) and measurement.get("publication_name") == name,
                 f"{label} {name} measurement is invalid")
        artifacts = publication.get("artifacts")
        expected_names = {f"{name}.bundle.json", f"{name}.hot.tgord", f"{name}.cold.tgord",
                          f"{name}.sidecar.ndjson"}
        _require(isinstance(artifacts, list) and {item.get("name") for item in artifacts} == expected_names,
                 f"{label} {name} artifact set differs")
        total = 0
        hot = None
        for artifact in artifacts:
            _digest(artifact.get("sha256"), f"{label} {name} artifact")
            size = _integer(artifact.get("bytes"), f"{label} {name} artifact bytes", minimum=1)
            identity = artifact.get("identity")
            _require(isinstance(identity, dict) and identity.get("size") == size and
                     _integer(identity.get("device"), "receipt device", minimum=1) > 0 and
                     _integer(identity.get("inode"), "receipt inode", minimum=1) > 0,
                     f"{label} {name} artifact identity is invalid")
            total += size
            if artifact["name"].endswith(".hot.tgord"):
                hot = size
        _require(measurement.get("artifact_bytes") == total and measurement.get("hot_artifact_bytes") == hot,
                 f"{label} {name} byte totals differ")
        bundle_artifact = next(item for item in artifacts if item["name"].endswith(".bundle.json"))
        _require(bundle_artifact["sha256"] == publication["bundle_sha256"],
                 f"{label} {name} bundle digest differs from its artifact")
        for field in ("manifest_digest", "dictionary_digest", "sidecar_digest",
                      "cold_payload_digest", "hot_index_digest"):
            _digest(measurement.get(field), f"{label} {name} {field}")
    return receipt


def _receipt_measurements(receipt: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return {item["publication_name"]: item["measurement"] for item in receipt["publications"]}


def _receipt_artifacts(receipt: dict[str, Any]) -> dict[str, dict[str, tuple[str, int]]]:
    return {
        publication["publication_name"]: {
            artifact["name"]: (artifact["sha256"], artifact["bytes"])
            for artifact in publication["artifacts"]
        }
        for publication in receipt["publications"]
    }


def _validate_candidate_raw(candidate: dict[str, Any], merged: dict[str, Any], samples_raw: bytes,
                            stats_raw: bytes) -> dict[str, Any]:
    _require(candidate.get("schema_version") == merged.get("schema_version") == 1 and
             candidate.get("status") == merged.get("status") == "smoke", "small-query report is incomplete")
    stripped = dict(merged)
    stripped.pop("service_peak_memory_bytes", None)
    configuration = dict(stripped.get("configuration", {}))
    configuration.pop("docker_stats_observations", None)
    stripped["configuration"] = configuration
    _require(stripped == candidate, "small-query merged result does not preserve the driver report")
    expected_configuration = {
        "cache_strategy": "warm", "concurrency": [1], "lock_sample_interval_ms": 10,
        "ramp_runs": 32, "runs_per_worker": 200,
        "task_concurrency_mode": "delegated_tasks_shared_root",
        "workload": "expense_detail/sales/ordered/limit-1",
    }
    _require(candidate.get("configuration") == expected_configuration,
             "small-query candidate configuration differs")
    cells = candidate.get("cells")
    expected_counts = {"business_sql": 200, "paired_snapshot": 200, "paired_plus_algebra": 200,
                       "full_history_ramp": 32, "full_history_hit": 200}
    _require(isinstance(cells, list) and len(cells) == 5, "small-query cell count differs")
    by_phase = {cell.get("phase"): cell for cell in cells}
    _require(set(by_phase) == set(expected_counts) and len(by_phase) == len(cells),
             "small-query cell phase set differs")

    rows: dict[str, list[dict[str, Any]]] = {phase: [] for phase in expected_counts}
    for line_number, line in enumerate(samples_raw.splitlines(), 1):
        _require(bool(line), f"small-query samples line {line_number} is blank")
        row = _decode_json(line, f"small-query samples line {line_number}")
        phase = row.get("phase")
        _require(phase in rows and row.get("concurrency") == 1, f"unknown small-query sample cell at line {line_number}")
        for field in ("worker", "iteration", "latency_ms", "rows", "actual_release_facts",
                      "actual_influence_facts", "charged_release_facts", "charged_influence_facts"):
            _number(row.get(field), f"small-query line {line_number} {field}", minimum=0)
        _require(row["charged_release_facts"] <= row["actual_release_facts"] and
                 row["charged_influence_facts"] <= row["actual_influence_facts"],
                 f"small-query charge exceeds actual at line {line_number}")
        rows[phase].append(row)
    _require(sum(map(len, rows.values())) == 832, "small-query raw sample count differs")

    for phase, expected_count in expected_counts.items():
        group, cell = rows[phase], by_phase[phase]
        _require(len(group) == expected_count == cell.get("samples"), f"small-query {phase} count differs")
        identities = {(row["worker"], row["iteration"]) for row in group}
        _require(len(identities) == len(group), f"small-query {phase} repeats worker/iteration")
        _validate_distribution(cell.get("latency_ms"), _distribution(row["latency_ms"] for row in group),
                               f"small-query {phase} latency")
        database_values = [row["database_ms"] for row in group if row.get("database_ms", 0) > 0]
        _validate_distribution(cell.get("database_ms"), _distribution(database_values),
                               f"small-query {phase} database")
        components = {name for row in group for name in row.get("component_ms", {})}
        _require(set(cell.get("component_ms", {})) == components, f"small-query {phase} components differ")
        for component in components:
            values = [row["component_ms"][component] for row in group if component in row.get("component_ms", {})]
            _validate_distribution(cell["component_ms"][component], _distribution(values),
                                   f"small-query {phase} component {component}")
        actual = sum(row["actual_release_facts"] + row["actual_influence_facts"] for row in group)
        charged = sum(row["charged_release_facts"] + row["charged_influence_facts"] for row in group)
        _require(cell.get("actual_facts") == actual and cell.get("charged_facts") == charged,
                 f"small-query {phase} fact totals differ")
        _close(cell.get("throughput_qps"), len(group) * 1000 / cell.get("elapsed_ms"),
               f"small-query {phase} throughput")
        if phase.startswith("full_history_"):
            _require(all(HEX64.fullmatch(str(row.get("observation_sha256", ""))) for row in group),
                     f"small-query {phase} observation digest is missing")
            fact_rate = (actual - charged) / actual
            query_rate = sum((row["actual_release_facts"] + row["actual_influence_facts"] > 0 and
                              row["charged_release_facts"] + row["charged_influence_facts"] == 0)
                             for row in group) / len(group)
            replay_rate = sum(row.get("semantic_replay") is True for row in group) / len(group)
            _close(cell.get("fact_history_hit_rate"), fact_rate, f"small-query {phase} fact hit rate")
            _close(cell.get("query_history_hit_rate"), query_rate, f"small-query {phase} query hit rate")
            _close(cell.get("semantic_replay_hit_rate"), replay_rate,
                   f"small-query {phase} semantic replay rate")

    units = {"B": 1, "KB": 1000, "MB": 1000**2, "GB": 1000**3,
             "KIB": 1024, "MIB": 1024**2, "GIB": 1024**3}
    peaks: dict[str, int] = {}
    observations: dict[str, int] = {}
    ansi = re.compile(rb"\x1b\[[0-9;?]*[ -/]*[@-~]")
    for line_number, line in enumerate(stats_raw.splitlines(), 1):
        cleaned = ansi.sub(b"", line).strip()
        row = _decode_json(cleaned, f"small-query Docker stats line {line_number}")
        name = row.get("Name", "")
        matches = [service for service in ("control-postgres", "business-postgres", "gateway")
                   if re.search(rf"(?:^|-){re.escape(service)}-\d+$", name)]
        _require(len(matches) == 1, f"small-query Docker stats line {line_number} has unknown service")
        match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)([A-Za-z]+)",
                             str(row.get("MemUsage", "")).split("/", 1)[0].strip().replace(" ", ""))
        _require(match is not None and match.group(2).upper() in units,
                 f"small-query Docker stats line {line_number} has invalid memory")
        value = round(float(match.group(1)) * units[match.group(2).upper()])
        service = matches[0]
        peaks[service] = max(peaks.get(service, 0), value)
        observations[service] = observations.get(service, 0) + 1
    _require(peaks == merged.get("service_peak_memory_bytes"), "small-query Docker memory peaks differ")
    _require(observations == merged["configuration"].get("docker_stats_observations"),
             "small-query Docker observation counts differ")
    hit = by_phase["full_history_hit"]
    _require(hit["samples"] == 200 and hit["fact_history_hit_rate"] == 1 and
             hit["query_history_hit_rate"] == 1 and hit["semantic_replay_hit_rate"] == 1 and
             hit["actual_facts"] == 1400 and hit["charged_facts"] == 0,
             "small-query history-hit contract differs")
    return {"hit": hit, "peaks": peaks, "sample_count": 832}


def _validate_report_summaries(report: dict[str, Any], measured: list[dict[str, Any]]) -> dict[tuple[str, str], dict[str, Any]]:
    groups: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for sample in measured:
        if sample["phase"] not in {"direct_sql", "novel", "semantic_replay"}:
            continue
        groups.setdefault((sample["phase"], ""), []).append(sample)
        groups.setdefault((sample["phase"], sample["shape"]), []).append(sample)
    summaries = report.get("summaries")
    _require(isinstance(summaries, list) and len(summaries) == 15, "full summary count differs")
    by_key = {(item.get("phase"), item.get("shape", "")): item for item in summaries}
    _require(len(by_key) == len(summaries) and set(by_key) == set(groups), "full summary cells differ")
    for key, rows in groups.items():
        summary = by_key[key]
        _require(summary.get("samples") == len(rows), f"full summary {key} sample count differs")
        _validate_distribution(summary.get("client_latency_ms"),
                               _distribution(row["client_latency_ms"] for row in rows),
                               f"full summary {key} latency")
        _validate_distribution(summary.get("database_ms"), _distribution(row["database_ms"] for row in rows),
                               f"full summary {key} database")
        components = {name for row in rows for name in row.get("component_ms", {})}
        _require(set(summary.get("component_ms", {})) == components, f"full summary {key} components differ")
        for component in components:
            values = [row["component_ms"][component] for row in rows if component in row.get("component_ms", {})]
            _validate_distribution(summary["component_ms"][component], _distribution(values),
                                   f"full summary {key} component {component}")
    return by_key


def validate_v4_evidence(repository_root: Path | str,
                         evidence_dir: Path | str | None = None) -> dict[str, Any]:
    """Validate all retained V4 evidence and return recomputed paper statistics.

    ``repository_root`` must be the repository root.  ``evidence_dir`` may
    point at an exact copy of ``evaluation/v4-acceptance/evidence``; source and
    the separately published baseline are still read from ``repository_root``.
    This override exists for hermetic mutation tests.  Any missing, additional,
    stale, malformed, non-canonical, or inconsistent retained artifact raises
    :class:`ValueError` before any statistics are returned.
    """

    root = Path(repository_root).resolve()
    _require((root / "go.mod").is_file(), "repository_root does not contain go.mod")
    override = None if evidence_dir is None else Path(evidence_dir)
    manifest, raw = _load_manifest(root, override)
    rel = lambda name: f"evaluation/v4-acceptance/evidence/{name}"
    result_raw = raw[rel("results.json")]
    config_raw = raw[rel("full-config.json")]
    environment_raw = raw[rel("environment.json")]
    activation_raw = raw[rel("activation-verification-receipt.json")]
    preflight_raw = raw[rel("preflight-artifact-verification-receipt.json")]
    candidate_raw = raw[rel("small-query-candidate.json")]
    candidate_results_raw = raw[rel("small-query-results.json")]
    candidate_samples_raw = raw[rel("small-query-samples.jsonl")]
    candidate_stats_raw = raw[rel("small-query-docker-stats.jsonl")]
    historical_provenance_raw = raw[rel("historical-source.json")]
    historical_archive_raw = raw[rel("historical-source-e8e751c.tar.gz")]
    baseline_raw = raw[BASELINE_REL.as_posix()]

    result = _decode_json(result_raw, "V4 results")
    config = _decode_json(config_raw, "V4 full config")
    environment = _decode_json(environment_raw, "V4 environment")
    candidate = _decode_json(candidate_raw, "V4 small-query candidate")
    candidate_results = _decode_json(candidate_results_raw, "V4 small-query merged result")
    baseline = _decode_json(baseline_raw, "small-query baseline")
    activation_receipt = _canonical_receipt(activation_raw, "activation verification receipt")
    preflight_receipt = _canonical_receipt(preflight_raw, "preflight verification receipt")

    source = _digest(manifest["acceptance_source_sha256"], "manifest source")
    _require(source == EXPECTED_HISTORICAL_SOURCE_SHA256 ==
             result.get("provenance", {}).get("source_sha256") ==
             environment.get("software", {}).get("acceptance_source_sha256"),
             "historical V4 source digest is not bound to results/environment/manifest")
    historical_source = _historical_source_snapshot(
        historical_provenance_raw, historical_archive_raw, source)
    current_source_relation = _current_source_relation(root, source)
    _require(_sha(config_raw) == result.get("provenance", {}).get("config_sha256"),
             "full config is not bound to results")
    env_sha = _sha(environment_raw)
    _require(env_sha == result.get("provenance", {}).get("environment_sha256") ==
             result.get("environment", {}).get("sha256") == config.get("environment_manifest", {}).get("sha256"),
             "environment is not cross-bound")
    _require(_sha(activation_raw) == result.get("provenance", {}).get("activation_verification_receipt_sha256"),
             "activation receipt is not bound to results")

    _require(environment.get("schema_version") == 1 and environment.get("project") == EXPECTED_CAMPAIGN,
             "environment identity differs")
    preflight_info = environment.get("datasets", {}).get("artifact_verification", {})
    _require(_sha(preflight_raw) == preflight_info.get("receipt_sha256") and
             preflight_receipt["receipt_body_sha256"] == preflight_info.get("receipt_body_sha256"),
             "preflight receipt is not bound to environment")
    activation_measurements = _receipt_measurements(activation_receipt)
    preflight_measurements = _receipt_measurements(preflight_receipt)
    _require(activation_measurements == preflight_measurements,
             "activation and preflight receipts disagree on publication measurements")
    _require(_receipt_artifacts(activation_receipt) == _receipt_artifacts(preflight_receipt),
             "activation and preflight receipts disagree on artifact name/hash/bytes")
    environment_publications = {item["name"]: item for item in preflight_info.get("publications", [])}
    _require(set(environment_publications) == set(preflight_measurements),
             "environment publication set differs from receipts")
    for name, measurement in preflight_measurements.items():
        published = environment_publications[name]
        for field in ("row_count", "manifest_digest", "dictionary_digest", "sidecar_digest",
                      "cold_payload_digest", "hot_index_digest", "artifact_bytes", "hot_artifact_bytes"):
            _require(published.get(field) == measurement.get(field),
                     f"environment publication {name} {field} differs from receipt")
    _require(preflight_info.get("total_artifact_bytes") ==
             sum(item["artifact_bytes"] for item in preflight_measurements.values()) and
             preflight_info.get("hot_artifact_bytes") ==
             sum(item["hot_artifact_bytes"] for item in preflight_measurements.values()) and
             preflight_info.get("receipt_bound_activation_runs") == 3,
             "environment receipt aggregate differs")

    catalog_sha = _sha(_read_regular(root / "evaluation/v4-acceptance/scale-fixture/catalog-full.yaml", 1 << 20))
    catalog = environment.get("software", {}).get("catalog", {})
    _require(catalog.get("host_sha256") == catalog.get("gateway_mount_sha256") ==
             catalog.get("control_cutover_digest") == catalog_sha, "environment Catalog digest differs")
    orchestration_paths = {
        "compose": "compose.yaml",
        "scale_overlay": "evaluation/v4-acceptance/compose.scale-narrow.yaml",
        "observer_overlay": "evaluation/v4-acceptance/compose.observer.yaml",
        "full_overlay": "evaluation/v4-acceptance/compose.full.yaml",
        "full_template": "evaluation/v4-acceptance/full-matrix.template.json",
        "provisioner": "evaluation/v4-acceptance/provision-full.sh",
        "dockerfile": "Dockerfile",
        "evaluation_dockerfile": "evaluation/Dockerfile",
    }
    reported_orchestration = environment.get("software", {}).get("orchestration_sha256", {})
    _require(set(reported_orchestration) == set(orchestration_paths),
             "environment orchestration artifact set differs")
    for name, relative in orchestration_paths.items():
        _require(reported_orchestration[name] == _sha(_read_regular(root / relative, 4 << 20)),
                 f"environment orchestration digest {name} differs")
    inputs = environment.get("datasets", {}).get("compiler_inputs", {})
    _require(inputs.get("scale_orders_v4_narrow_1_sha256") == _sha(_read_regular(
        root / "evaluation/v4-acceptance/scale-fixture/snapshots/scale-orders-v4-narrow-1.json", 1 << 20)),
        "environment orders compiler input digest differs")
    _require(inputs.get("scale_lineitem_v4_narrow_1_sha256") == _sha(_read_regular(
        root / "evaluation/v4-acceptance/scale-fixture/snapshots/scale-lineitem-v4-narrow-1.json", 1 << 20)),
        "environment lineitem compiler input digest differs")
    counts = environment.get("datasets", {}).get("counts", {})
    _require(counts == {"scale_orders_rows": 50000, "scale_lineitem_rows": 250000,
                        "scale_orders_sidecar_rows": 50000, "scale_lineitem_sidecar_rows": 250000,
                        "installed_publications": 2}, "environment dataset counts differ")
    startup = environment.get("software", {}).get("startup_containers", {})
    for name in ("snapshot_index_orders", "snapshot_index_lineitem"):
        one = startup.get(name, {})
        _require(one.get("exit_code") == 0 and one.get("oom_killed") is False and
                 one.get("memory_limit_bytes") == 4 << 30, f"environment {name} did not pass its 4-GiB build")
    gateway_startup = startup.get("gateway_startup_cgroup", {})
    _require(gateway_startup.get("scope") == "private-cgroup-v2-including-mmap" and
             gateway_startup.get("memory_swap_max_bytes") == 0 and
             all(value == 0 for value in gateway_startup.get("memory_events", {}).values()),
             "environment Gateway cgroup evidence is invalid")

    _require(config.get("schema_version") == 1 and config.get("require_fresh_root") is True and
             config.get("request_timeout_ms") == 30000 and config.get("statement_timeout_ms") == 15000 and
             config.get("overlap_tolerance_percentage_points") == 0.01,
             "full config controls differ")
    cases = config.get("cases")
    _require(isinstance(cases, list) and len(cases) == 7, "full config must contain seven cases")
    case_map = {case.get("id"): case for case in cases}
    _require(len(case_map) == 7, "full config repeats a case")
    expected_cases = {
        "join-group-max-point-overlap-0": ("join_group", 0, 0, False),
        "join-group-max-point-overlap-50": ("join_group", 50, 1, False),
        "join-group-max-point-overlap-90": ("join_group", 90, 5, False),
        "join-group-max-point-overlap-100": ("join_group", 100, 1, False),
        "scan-scale-overlap-0": ("scan", 0, 0, True),
        "page-scale-overlap-0": ("page", 0, 0, True),
        "union-scale-overlap-0": ("union", 0, 0, True),
    }
    _require(set(case_map) == set(expected_cases), "full config case identities differ")
    all_tasks: set[str] = set()
    expected_samples: dict[tuple[str, int], tuple[str, set[str]]] = {}
    for case_id, (shape, overlap, setups, small) in expected_cases.items():
        case = case_map[case_id]
        _require(case.get("shape") == shape and case.get("target_overlap_percent") == overlap and
                 case.get("overlap_dimension") == "influence" and bool(case.get("small_query", False)) == small and
                 len(case.get("setup_plans", [])) == setups and isinstance(case.get("plan"), dict) and
                 isinstance(case.get("direct_sql"), str) and case["direct_sql"],
                 f"full config case {case_id} differs")
        tasks = case.get("task_ids")
        _require(isinstance(tasks, list) and len(tasks) == 20 and
                 all(isinstance(task, str) and TASK_ID.fullmatch(task) for task in tasks),
                 f"full config case {case_id} task pool differs")
        _require(not (all_tasks & set(tasks)), f"full config case {case_id} reuses a root")
        all_tasks.update(tasks)
        phases = {"direct_sql", "novel", "semantic_replay"} | {f"setup_{index}" for index in range(1, setups + 1)}
        for trial, task in enumerate(tasks, 1):
            expected_samples[(case_id, trial)] = (hashlib.sha256(task.encode()).hexdigest(), phases)
    _require(len(all_tasks) == 140, "full config does not contain 140 fresh roots")

    report_config = result.get("configuration", {})
    _require(report_config == {"gateway_url": "http://gateway:8082", "request_timeout_ms": 30000,
                               "statement_timeout_ms": 15000,
                               "overlap_tolerance_percentage_points": 0.01, "require_fresh_root": True,
                               "case_count": 7, "trial_count": 140,
                               "configured_shapes": ["join_group", "page", "scan", "union"],
                               "configured_overlap_percentages": [0, 50, 90, 100]},
             "reported configuration differs from fixed campaign")
    _require(result.get("schema_version") == 1 and result.get("status") == "complete_measured_campaign" and
             result.get("acceptance") == "pass" and not result.get("errors") and not result.get("warnings"),
             "full campaign did not complete cleanly")
    gates_list = result.get("gates")
    _require(isinstance(gates_list, list) and len(gates_list) == 30, "full campaign gate count differs")
    gates = {gate.get("id"): gate for gate in gates_list}
    _require(len(gates) == 30 and set(gates) == EXPECTED_GATES and
             all(gate.get("status") == "pass" and not gate.get("reason") for gate in gates_list),
             "full campaign gate set/status differs")
    _require(gates["evidence_provenance"].get("evidence") == result.get("provenance") and
             gates["fixed_environment_manifest"].get("evidence") == result.get("environment") and
             gates["required_observer"].get("evidence") == {"missing_samples": 0},
             "provenance/environment/observer gate evidence differs")

    samples = result.get("samples")
    _require(isinstance(samples, list) and len(samples) == 560, "full campaign must contain 560 samples")
    seen_samples: set[tuple[str, int, str]] = set()
    grouped: dict[tuple[str, int], dict[str, dict[str, Any]]] = {key: {} for key in expected_samples}
    phase_counts: dict[str, int] = {}
    for index, sample in enumerate(samples):
        _require(isinstance(sample, dict), f"sample {index} is not an object")
        case_id, trial, phase = sample.get("case_id"), sample.get("trial"), sample.get("phase")
        key = (case_id, trial)
        _require(key in expected_samples and phase in expected_samples[key][1], f"sample {index} identity differs")
        identity = (case_id, trial, phase)
        _require(identity not in seen_samples, f"sample {index} repeats {identity}")
        seen_samples.add(identity)
        expected_task, _ = expected_samples[key]
        case = case_map[case_id]
        _require(sample.get("task_sha256") == expected_task and sample.get("shape") == case["shape"] and
                 sample.get("target_overlap_percent") == case["target_overlap_percent"] and
                 sample.get("overlap_dimension") == "influence" and sample.get("status") == "measured",
                 f"sample {index} metadata differs")
        _number(sample.get("client_latency_ms"), f"sample {index} client latency", minimum=0)
        _number(sample.get("database_ms"), f"sample {index} database time", minimum=0)
        _digest(sample.get("result_sha256"), f"sample {index} result")
        observer = sample.get("observer", {})
        _require(observer.get("status") == "measured" and
                 observer.get("memory_scope") == "cgroup_v2_memory_peak_including_mmap" and
                 "business_sql_queries_total" in observer.get("delta", {}) and
                 "gateway_network_rx_bytes" in observer.get("delta", {}) and
                 "gateway_network_tx_bytes" in observer.get("delta", {}) and
                 "gateway_memory_peak_bytes" in observer.get("after", {}),
                 f"sample {index} observer evidence differs")
        wal = sample.get("wal", {})
        _require(wal.get("status") == "measured" and _integer(wal.get("business_bytes"), "WAL", minimum=0) >= 0 and
                 _integer(wal.get("control_bytes"), "WAL", minimum=0) >= 0,
                 f"sample {index} WAL evidence differs")
        grouped[key][phase] = sample
        phase_counts[phase] = phase_counts.get(phase, 0) + 1

    _require(phase_counts == {"direct_sql": 140, "novel": 140, "semantic_replay": 140,
                              "setup_1": 60, "setup_2": 20, "setup_3": 20,
                              "setup_4": 20, "setup_5": 20}, "full campaign phase coverage differs")
    max_novel: list[dict[str, Any]] = []
    max_replay: list[dict[str, Any]] = []
    max_direct: list[dict[str, Any]] = []
    overlap_values: dict[int, list[float]] = {0: [], 50: [], 90: [], 100: []}
    for key, phases in grouped.items():
        _require(set(phases) == expected_samples[key][1], f"root {key} phase set differs")
        direct, novel, replay = phases["direct_sql"], phases["novel"], phases["semantic_replay"]
        expected = case_map[key[0]].get("expected", {})
        _require(direct.get("row_count") == expected.get("row_count"),
                 f"root {key} row count differs from config")
        _require(direct["result_sha256"] == novel["result_sha256"] == replay["result_sha256"] and
                 direct.get("row_count") == novel.get("row_count") == replay.get("row_count"),
                 f"root {key} result identity differs")
        for label, sample in (("novel", novel), ("replay", replay)):
            exposure = sample.get("exposure")
            _require(isinstance(exposure, dict) and exposure.get("profile_version") == "taskgate-exposure-v4",
                     f"root {key} {label} exposure profile differs")
            for field in ("observation_sha256", "dictionary_set_digest", "release_set_sha256",
                          "influence_set_sha256", "outcome_set_sha256"):
                _digest(exposure.get(field), f"root {key} {label} {field}")
            for dimension in ("release", "influence", "outcome"):
                actual = _integer(exposure.get(f"actual_{dimension}_facts"), "actual facts", minimum=0)
                charged = _integer(exposure.get(f"charged_{dimension}_facts"), "charged facts", minimum=0)
                _require(charged <= actual, f"root {key} {label} charge exceeds actual")
        novel_exp, replay_exp = novel["exposure"], replay["exposure"]
        _require(novel.get("semantic_replay", False) is False,
                 f"root {key} novel request was marked semantic replay")
        for config_field, exposure_field in (("release_facts", "actual_release_facts"),
                                             ("influence_facts", "actual_influence_facts"),
                                             ("outcome_facts", "actual_outcome_facts")):
            if config_field in expected:
                _require(novel_exp.get(exposure_field) == expected[config_field] and
                         replay_exp.get(exposure_field) == expected[config_field],
                         f"root {key} {config_field} differs from config")
        for field in ("actual_release_facts", "actual_influence_facts", "actual_outcome_facts",
                      "observation_sha256", "dictionary_set_digest", "release_set_sha256",
                      "influence_set_sha256", "outcome_set_sha256", "root_epoch"):
            _require(novel_exp.get(field) == replay_exp.get(field), f"root {key} replay {field} differs")
        _require(replay.get("semantic_replay") is True and replay.get("database_ms") == 1 and
                 all(replay_exp.get(field) == 0 for field in ("charged_release_facts",
                                                               "charged_influence_facts",
                                                               "charged_outcome_facts")) and
                 replay["observer"]["delta"]["business_sql_queries_total"] == 0 and
                 "business_postgresql" not in replay.get("component_ms", {}) and
                 "provenance_postgresql" not in replay.get("component_ms", {}),
                 f"root {key} replay is not a zero-charge/no-SQL semantic hit")
        overlap = int(case_map[key[0]]["target_overlap_percent"])
        actual_influence = novel_exp["actual_influence_facts"]
        expected_charge = actual_influence * (100 - overlap) // 100
        _require(actual_influence * (100 - overlap) % 100 == 0 and
                 novel_exp["charged_influence_facts"] == expected_charge,
                 f"root {key} exact influence overlap differs")
        observed = 100 * (actual_influence - expected_charge) / actual_influence
        _close(novel.get("observed_overlap_percent"), observed, f"root {key} observed overlap")
        overlap_values[overlap].append(observed)
        exact_effects = {
            "join-group-max-point-overlap-0": ((12, 1_035_000, 1), (12, 1_035_000, 1)),
            "join-group-max-point-overlap-50": ((12, 1_035_000, 1), (12, 517_500, 1)),
            "join-group-max-point-overlap-90": ((12, 1_035_000, 1), (12, 103_500, 1)),
            "join-group-max-point-overlap-100": ((12, 1_035_000, 1), (0, 0, 1)),
            "scan-scale-overlap-0": ((2, 3, 1), (2, 3, 1)),
            "page-scale-overlap-0": ((60, 80, 1), (60, 80, 1)),
            "union-scale-overlap-0": ((80, 120, 1), (80, 120, 1)),
        }
        actual_effect, charged_effect = exact_effects[key[0]]
        _require(tuple(novel_exp[f"actual_{dimension}_facts"] for dimension in
                       ("release", "influence", "outcome")) == actual_effect and
                 tuple(novel_exp[f"charged_{dimension}_facts"] for dimension in
                       ("release", "influence", "outcome")) == charged_effect,
                 f"root {key} exact novel effect differs")
        if novel["shape"] == "join_group":
            _require((novel_exp["actual_release_facts"], novel_exp["actual_influence_facts"],
                      novel_exp["actual_outcome_facts"]) == MAX_POINT,
                     f"root {key} maximum-point facts differ")
            max_novel.append(novel)
            max_replay.append(replay)
            max_direct.append(direct)
    _require({key: len(value) for key, value in overlap_values.items()} == {0: 80, 50: 20, 90: 20, 100: 20},
             "full campaign overlap sample counts differ")
    _require(len(max_novel) == len(max_replay) == len(max_direct) == 80,
             "maximum-point sample count differs")
    coverage = result.get("coverage", {})
    reported_overlaps = coverage.get("overlaps", {})
    _require(set(reported_overlaps) == {"0", "50", "90", "100"},
             "reported overlap coverage set differs")
    for overlap, values in overlap_values.items():
        _require(reported_overlaps[str(overlap)] == {
            "status": "measured", "samples": len(values), "observed_values": values},
            f"reported overlap-{overlap} coverage differs")
    reported_shapes = coverage.get("shapes", {})
    expected_shape_coverage = {"scan": 20, "join_group": 80, "union": 20, "page": 20}
    _require(set(reported_shapes) == set(expected_shape_coverage), "reported shape coverage set differs")
    for shape, count in expected_shape_coverage.items():
        _require(reported_shapes[shape] == {"status": "measured", "samples": count},
                 f"reported shape-{shape} coverage differs")

    summaries = _validate_report_summaries(result, samples)
    novel_dist = _distribution(sample["client_latency_ms"] for sample in max_novel)
    replay_dist = _distribution(sample["client_latency_ms"] for sample in max_replay)
    direct_dist = _distribution(sample["client_latency_ms"] for sample in max_direct)
    _validate_distribution(gates["novel_latency"].get("evidence"), novel_dist, "novel gate")
    _validate_distribution(gates["semantic_replay_latency"].get("evidence"), replay_dist, "replay gate")
    _require(novel_dist["p50"] <= 3000 and novel_dist["p95"] <= 4000 and
             replay_dist["p50"] <= 100 and replay_dist["p95"] <= 150,
             "online latency SLO is not met")
    _require(gates["execution_integrity"].get("evidence") == {"failed_samples": 0, "total_samples": 560},
             "execution-integrity evidence differs")
    for overlap, values in overlap_values.items():
        _require(gates[f"overlap_{overlap}"].get("evidence") == values,
                 f"overlap-{overlap} gate evidence differs")
    for shape, pairs in {"scan": 20, "join_group": 80, "union": 20, "page": 20}.items():
        _require(gates[f"shape_{shape}"].get("evidence") == {"pairs": pairs},
                 f"shape-{shape} gate evidence differs")

    observed_peaks = [sample["observer"]["after"]["gateway_memory_peak_bytes"] for sample in samples
                      if sample["phase"] not in {"direct_sql"} and not sample["phase"].startswith("setup_")]
    gateway_peak = max(observed_peaks)
    _require(gateway_peak <= 512 << 20 and
             gates["gateway_cgroup_peak_memory"].get("evidence") == {
                 "max_bytes": gateway_peak, "scope": "cgroup_v2_memory_peak_including_mmap"} and
             gateway_startup.get("memory_peak_bytes") == gateway_peak,
             "Gateway cgroup peak evidence differs or exceeds 512 MiB")
    replay_sql = gates["semantic_replay_no_business_sql"].get("evidence", {})
    _require(replay_sql.get("samples") == 140 and replay_sql.get("missing") == 0 and
             replay_sql.get("violations") == 0 and replay_sql.get("deltas") == [0] * 140,
             "replay no-Business-SQL gate differs")
    replay_components = gates["semantic_replay_gateway_sql_components"].get("evidence", {})
    _require(replay_components == {"samples": 140, "violations": 0},
             "replay Gateway SQL-component gate differs")
    network_rx = sum(sample["observer"]["delta"]["gateway_network_rx_bytes"] for sample in samples)
    network_tx = sum(sample["observer"]["delta"]["gateway_network_tx_bytes"] for sample in samples)
    _require(gates["network_measurement"].get("evidence") == {
        "samples": 560, "rx_bytes": network_rx, "tx_bytes": network_tx},
        "network gate does not reproduce from samples")
    wal_business = sum(sample["wal"]["business_bytes"] for sample in samples)
    wal_control = sum(sample["wal"]["control_bytes"] for sample in samples)
    _require(gates["wal_measurement"].get("evidence") == {
        "samples": 560, "missing": 0, "business_bytes": wal_business, "control_bytes": wal_control},
        "WAL gate does not reproduce from samples")

    index = result.get("index_build", {})
    _require(index.get("status") == "measured" and len(index.get("runs", [])) == 1,
             "index build measurement differs")
    index_run = index["runs"][0]
    index_ms = _number(index_run.get("wall_ms"), "index build wall", minimum=0)
    builder_rss = _integer(index_run.get("root_peak_rss_bytes"), "index builder RSS", minimum=1)
    _require(index_run.get("status") == "measured" and index_run.get("exit_code") == 0 and
             index_ms <= 600000 and builder_rss <= 4 << 30, "index build SLO is not met")
    _require(gates["index_build_time"].get("evidence") == {"wall_ms": [index_ms]} and
             gates["index_builder_rss"].get("evidence", {}).get("root_process_peak_rss_bytes") == [builder_rss],
             "index build gate evidence differs")
    artifacts = result.get("artifacts", {})
    artifact_total = _integer(artifacts.get("total_bytes"), "artifact total", minimum=1)
    artifact_hot = _integer(artifacts.get("hot_bytes"), "artifact hot", minimum=1)
    _require(artifacts.get("status") == "measured" and artifact_total <= 2 << 30 and artifact_hot <= 160 << 20 and
             index_run.get("artifact_bytes") == artifact_total and index_run.get("hot_artifact_bytes") == artifact_hot and
             sum(item["artifact_bytes"] for item in preflight_measurements.values()) == artifact_total and
             sum(item["hot_artifact_bytes"] for item in preflight_measurements.values()) == artifact_hot,
             "artifact size evidence differs or exceeds its SLO")
    _require(gates["artifact_total"].get("evidence") == {"bytes": artifact_total, "limit_bytes": 2 << 30} and
             gates["artifact_hot"].get("evidence") == {"bytes": artifact_hot, "limit_bytes": 160 << 20},
             "artifact gate evidence differs")
    verification = result.get("activation_verification", {})
    _require(verification.get("status") == "measured" and len(verification.get("runs", [])) == 1 and
             verification["runs"][0].get("status") == "measured" and
             verification["runs"][0].get("exit_code") == 0,
             "strict activation verification differs")
    verification_ms = _number(verification["runs"][0].get("wall_ms"), "strict verification wall", minimum=0)
    verification_gate = gates["activation_strict_verification"].get("evidence", {})
    _require(verification_gate.get("receipt_sha256") == _sha(activation_raw) and
             verification_gate.get("wall_ms") == [verification_ms],
             "strict-verification gate evidence differs")
    activation = result.get("activation", {})
    activation_runs = activation.get("runs", [])
    _require(activation.get("status") == "measured" and len(activation_runs) == 3 and
             all(run.get("status") == "measured" and run.get("exit_code") == 0 and
                 _number(run.get("wall_ms"), "activation wall", minimum=0) <= 2000 for run in activation_runs),
             "warm activation SLO is not met")
    activation_ms = [float(run["wall_ms"]) for run in activation_runs]
    _require(gates["activation_time"].get("evidence") == {"wall_ms": activation_ms},
             "activation gate evidence differs")
    storage = result.get("storage", {})
    _require(storage.get("status") == "measured" and storage.get("artifact_bytes") == artifact_total and
             storage.get("measured_roots") == 140 and
             [item.get("roots") for item in storage.get("amortized_1_10_100_roots", [])] == [1, 10, 100],
             "V4 storage evidence differs")
    _require(gates["storage_measurement"].get("evidence") == storage,
             "storage gate evidence differs")
    component_by_shape = {
        (shape or "all"): summary["component_ms"]
        for (phase, shape), summary in summaries.items() if phase == "novel"
    }
    expected_bitmap = {shape: values["bitmap_derivation"] for shape, values in component_by_shape.items()}
    expected_stream = {shape: values["ordinal_stream"] for shape, values in component_by_shape.items()}
    expected_provenance = {shape: values["provenance_postgresql"] for shape, values in component_by_shape.items()}
    expected_settle = {shape: values["settle_persist"] for shape, values in component_by_shape.items()}
    _require(gates["bitmap_derivation_end_to_end"].get("evidence") == expected_bitmap,
             "bitmap-derivation gate differs")
    _require(gates["ordinal_stream_end_to_end"].get("evidence") == {
        "provenance_postgresql_ms": expected_provenance,
        "bitmap_derivation_ms": expected_bitmap, "ordinal_stream_ms": expected_stream},
        "ordinal-stream gate differs")
    _require(gates["settlement_measurement"].get("evidence") == expected_settle,
             "settlement gate differs")

    candidate_stats = _validate_candidate_raw(candidate, candidate_results, candidate_samples_raw, candidate_stats_raw)
    _require(baseline.get("schema_version") == 2 and baseline.get("status") == "complete_controlled_local_campaign",
             "small-query baseline is incomplete")
    baseline_cells = {(item.get("phase"), item.get("concurrency")): item for item in baseline.get("cells", [])}
    baseline_hit = baseline_cells.get(("full_history_hit", 1), {})
    candidate_hit = candidate_stats["hit"]
    baseline_ref, candidate_ref = config.get("small_query_baseline", {}), config.get("small_query_candidate", {})
    _require(baseline_ref.get("artifact_sha256") == _sha(baseline_raw) and
             candidate_ref.get("artifact_sha256") == _sha(candidate_raw),
             "small-query artifacts are not bound by full config")
    for reference, cell, label in ((baseline_ref, baseline_hit, "baseline"),
                                   (candidate_ref, candidate_hit, "candidate")):
        _close(reference.get("p50_ms"), cell.get("latency_ms", {}).get("p50"), f"small-query {label} p50")
        _close(reference.get("throughput_qps"), cell.get("throughput_qps"),
               f"small-query {label} throughput")
    small_gate = gates["small_query_regression"].get("evidence", {})
    baseline_p50 = float(baseline_hit["latency_ms"]["p50"])
    candidate_p50 = float(candidate_hit["latency_ms"]["p50"])
    baseline_qps = float(baseline_hit["throughput_qps"])
    candidate_qps = float(candidate_hit["throughput_qps"])
    latency_change = (candidate_p50 / baseline_p50 - 1) * 100
    throughput_degradation = (1 - candidate_qps / baseline_qps) * 100
    _require(latency_change <= 10 and throughput_degradation <= 10,
             "small-query regression exceeds 10 percent")
    expected_small = {
        "baseline_artifact_sha256": _sha(baseline_raw), "baseline_p50_ms": baseline_p50,
        "baseline_throughput_qps": baseline_qps, "candidate_artifact_sha256": _sha(candidate_raw),
        "candidate_p50_ms": candidate_p50, "candidate_throughput_qps": candidate_qps,
        "latency_degradation_percent": latency_change, "limit_percent": 10,
        "throughput_degradation_percent": throughput_degradation,
    }
    _require(set(small_gate) == set(expected_small), "small-query gate fields differ")
    for key, value in expected_small.items():
        if isinstance(value, float):
            _close(small_gate.get(key), value, f"small-query gate {key}")
        else:
            _require(small_gate.get(key) == value, f"small-query gate {key} differs")
    env_small = environment.get("datasets", {}).get("small_query_comparison", {})
    _require(env_small.get("baseline_sha256") == _sha(baseline_raw) and
             env_small.get("candidate_sha256") == _sha(candidate_raw) and
             env_small.get("candidate_samples") == 200 and env_small.get("actual_facts") == 1400 and
             env_small.get("charged_facts") == 0 and env_small.get("query_history_hit_rate") == 1 and
             env_small.get("fact_history_hit_rate") == 1 and env_small.get("semantic_replay_hit_rate") == 1,
             "environment small-query comparison differs")

    join_novel_summary = summaries[("novel", "join_group")]
    components = join_novel_summary["component_ms"]
    stats = {
        "profile_version": "taskgate-exposure-v4",
        "source_sha256": source,
        "results_sha256": _sha(result_raw),
        "environment_sha256": env_sha,
        "gate_count": 30,
        "sample_count": 560,
        "case_count": 7,
        "root_count": 140,
        "shape_count": 4,
        "overlap_count": 4,
        "max_point_samples": 80,
        "max_release_facts": MAX_POINT[0],
        "max_influence_facts": MAX_POINT[1],
        "max_outcome_facts": MAX_POINT[2],
        "direct_p50_ms": direct_dist["p50"],
        "direct_p95_ms": direct_dist["p95"],
        "novel_p50_ms": novel_dist["p50"],
        "novel_p95_ms": novel_dist["p95"],
        "replay_p50_ms": replay_dist["p50"],
        "replay_p95_ms": replay_dist["p95"],
        "novel_speedup_over_v3": 169300 / float(novel_dist["p50"]),
        "novel_latency_reduction_percent": (1 - float(novel_dist["p50"]) / 169300) * 100,
        "replay_speedup_over_v3": 154100 / float(replay_dist["p50"]),
        "novel_over_direct_ratio": float(novel_dist["p50"]) / float(direct_dist["p50"]),
        "replay_no_business_sql_samples": 140,
        "network_rx_bytes": network_rx,
        "network_tx_bytes": network_tx,
        "wal_business_bytes": wal_business,
        "wal_control_bytes": wal_control,
        "gateway_peak_bytes": gateway_peak,
        "index_build_ms": index_ms,
        "index_builder_peak_rss_bytes": builder_rss,
        "artifact_total_bytes": artifact_total,
        "artifact_hot_bytes": artifact_hot,
        "activation_verification_ms": verification_ms,
        "activation_ms": activation_ms,
        "activation_max_ms": max(activation_ms),
        "storage_fixed_control_bytes": storage["fixed_control_bytes"],
        "storage_runtime_control_bytes": storage["runtime_control_bytes"],
        "storage_amortized_1_10_100_roots": storage["amortized_1_10_100_roots"],
        "bitmap_derivation_p50_ms": components["bitmap_derivation"]["p50"],
        "ordinal_stream_p50_ms": components["ordinal_stream"]["p50"],
        "provenance_postgresql_p50_ms": components["provenance_postgresql"]["p50"],
        "settlement_p50_ms": components["settle_persist"]["p50"],
        "small_query_raw_samples": candidate_stats["sample_count"],
        "small_query_hit_samples": candidate_hit["samples"],
        "small_query_baseline_p50_ms": baseline_p50,
        "small_query_candidate_p50_ms": candidate_p50,
        "small_query_baseline_qps": baseline_qps,
        "small_query_candidate_qps": candidate_qps,
        "small_query_latency_change_percent": latency_change,
        "small_query_latency_improvement_percent": -latency_change,
        "small_query_throughput_degradation_percent": throughput_degradation,
        "small_query_throughput_improvement_percent": -throughput_degradation,
        "small_query_gateway_peak_bytes": candidate_stats["peaks"]["gateway"],
        "source_evidence_mode": historical_source["mode"],
        "historical_git_commit": historical_source["git_commit"],
        "historical_git_tree": historical_source["git_tree"],
        "historical_source_file_count": historical_source["source_file_count"],
        "historical_source_archive_sha256": historical_source["archive_sha256"],
        "current_source_sha256": current_source_relation["current_source_sha256"],
        "current_source_relation": current_source_relation["status"],
        "current_source_matches_historical": current_source_relation["matches_historical"],
    }
    return {
        "manifest": manifest,
        "historical_source": historical_source,
        "current_source_relation": current_source_relation,
        "report": result,
        "config": config,
        "environment": environment,
        "candidate": candidate_results,
        "baseline": baseline,
        "stats": stats,
    }


def validate_v4(repository_root: Path | str,
                evidence_dir: Path | str | None = None) -> dict[str, Any]:
    """Backward-friendly short alias for :func:`validate_v4_evidence`."""

    return validate_v4_evidence(repository_root, evidence_dir)


__all__ = ["validate_v4", "validate_v4_evidence"]
