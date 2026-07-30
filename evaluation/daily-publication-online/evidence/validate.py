#!/usr/bin/env python3
"""Fail-closed validator for the compact 345k RQ5 online evidence pack."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import pathlib
import stat
import sys
from types import ModuleType
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
REPOSITORY_ROOT = HERE.parents[2]
RUN_ID = "scale-20260730-final"
ROWS = 345_000
DAYS = ("day0", "day1", "day2", "day3")
PACK_SCHEMA = "taskgate-daily-publication-online-pack-v1"
DEFAULT_PACK = HERE / RUN_ID
PAPER_VALIDATOR = REPOSITORY_ROOT / "paper/tkde/rq5_evidence.py"
MAX_DESCRIPTOR_BYTES = 16 << 20
HEX64 = frozenset("0123456789abcdef")


class EvidenceError(ValueError):
    """Raised when the compact pack is absent, mutable, or inconsistent."""


def _require(condition: bool, message: str) -> None:
    if not condition:
        raise EvidenceError("online evidence pack: " + message)


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def canonical_bytes(value: Any) -> bytes:
    return json.dumps(
        value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
    ).encode("utf-8")


def pretty_bytes(value: Any) -> bytes:
    return (
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    ).encode("utf-8")


def _reject_duplicate_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise EvidenceError(f"online evidence pack: duplicate JSON key {key!r}")
        result[key] = value
    return result


def _reject_nonfinite(value: str) -> Any:
    raise EvidenceError(f"online evidence pack: non-finite JSON number {value!r}")


def read_regular(path: pathlib.Path) -> bytes:
    try:
        before = os.lstat(path)
    except OSError as exc:
        raise EvidenceError(f"online evidence pack: cannot stat {path}: {exc}") from exc
    _require(
        stat.S_ISREG(before.st_mode) and not stat.S_ISLNK(before.st_mode),
        f"{path} must be a regular non-symlink file",
    )
    _require(
        0 < before.st_size <= MAX_DESCRIPTOR_BYTES,
        f"{path} has invalid descriptor size {before.st_size}",
    )
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "rb") as source:
            opened = os.fstat(source.fileno())
            body = source.read(MAX_DESCRIPTOR_BYTES + 1)
            after = os.fstat(source.fileno())
        after_path = os.lstat(path)
    except OSError as exc:
        raise EvidenceError(f"online evidence pack: cannot read {path}: {exc}") from exc
    identity = lambda value: (value.st_dev, value.st_ino, value.st_size, value.st_mtime_ns)
    _require(
        identity(before) == identity(opened) == identity(after) == identity(after_path),
        f"{path} changed while being read",
    )
    _require(len(body) == before.st_size, f"{path} changed size while being read")
    return body


def load_json(path: pathlib.Path) -> tuple[dict[str, Any], bytes]:
    body = read_regular(path)
    try:
        value = json.loads(
            body.decode("utf-8"),
            object_pairs_hook=_reject_duplicate_pairs,
            parse_constant=_reject_nonfinite,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise EvidenceError(f"online evidence pack: invalid JSON in {path}: {exc}") from exc
    _require(isinstance(value, dict), f"{path} must contain one JSON object")
    return value, body


def publication_name(day: str) -> str:
    return f"daily-lineitem-{day}-r{ROWS}"


def required_paths() -> tuple[str, ...]:
    paths = ["online-evidence.json", "dataset-manifest.json", "preparation.json"]
    paths.extend(f"approved-inputs/{day}.json" for day in DAYS)
    paths.extend(f"catalogs/{day}.yaml" for day in DAYS)
    paths.extend(
        f"artifacts/{publication_name(day)}/{publication_name(day)}.bundle.json"
        for day in DAYS
    )
    return tuple(sorted(paths))


def role_for(relative: str) -> str:
    if relative == "online-evidence.json":
        return "online-transition-evidence"
    if relative == "dataset-manifest.json":
        return "dataset-manifest"
    if relative == "preparation.json":
        return "preparation-manifest"
    if relative.startswith("approved-inputs/"):
        return "approved-input"
    if relative.startswith("catalogs/"):
        return "catalog"
    if relative.endswith(".bundle.json"):
        return "bundle-manifest"
    raise EvidenceError(f"online evidence pack: unclassified path {relative}")


def _exact_fields(value: Any, expected: set[str], label: str) -> dict[str, Any]:
    _require(isinstance(value, dict), f"{label} must be an object")
    _require(
        set(value) == expected,
        f"{label} fields differ: {sorted(set(value) ^ expected)}",
    )
    return value


def _digest(value: Any, label: str) -> str:
    _require(
        isinstance(value, str)
        and len(value) == 64
        and all(character in HEX64 for character in value),
        f"{label} must be a lowercase SHA-256",
    )
    return value


def _integer(value: Any, label: str, minimum: int = 0) -> int:
    _require(
        isinstance(value, int) and not isinstance(value, bool) and value >= minimum,
        f"{label} must be an integer >= {minimum}",
    )
    return value


def _expected_directories() -> set[str]:
    result: set[str] = set()
    for relative in (*required_paths(), "pack-manifest.json"):
        parent = pathlib.PurePosixPath(relative).parent
        while parent.as_posix() != ".":
            result.add(parent.as_posix())
            parent = parent.parent
    return result


def _actual_tree(pack: pathlib.Path) -> tuple[set[str], set[str]]:
    files: set[str] = set()
    directories: set[str] = set()
    for path in pack.rglob("*"):
        relative = path.relative_to(pack).as_posix()
        try:
            mode = os.lstat(path).st_mode
        except OSError as exc:
            raise EvidenceError(f"online evidence pack: cannot stat {path}: {exc}") from exc
        _require(not stat.S_ISLNK(mode), f"pack member {relative} must not be a symlink")
        if stat.S_ISREG(mode):
            files.add(relative)
        elif stat.S_ISDIR(mode):
            directories.add(relative)
        else:
            raise EvidenceError(f"online evidence pack: unsupported member {relative}")
    return files, directories


def _expected_omissions(pack: pathlib.Path) -> list[dict[str, Any]]:
    values: list[dict[str, Any]] = []
    for day in DAYS:
        name = publication_name(day)
        relative = f"artifacts/{name}/{name}.bundle.json"
        bundle, _ = load_json(pack / relative)
        _require(bundle.get("publication_name") == name, f"{relative} publication differs")
        for role, suffix in (
            ("hot", ".hot.tgord"),
            ("cold", ".cold.tgord"),
            ("sidecar", ".sidecar.ndjson"),
        ):
            descriptor = _exact_fields(
                bundle.get(role), {"name", "bytes", "sha256"}, f"{relative}.{role}"
            )
            _require(
                descriptor["name"] == name + suffix,
                f"{relative}.{role} name differs",
            )
            artifact_bytes = _integer(
                descriptor["bytes"], f"{relative}.{role}.bytes", minimum=1
            )
            artifact_sha = _digest(
                descriptor["sha256"], f"{relative}.{role}.sha256"
            )
            values.append(
                {
                    "artifact_path": f"artifacts/{name}/{descriptor['name']}",
                    "bundle_manifest_path": relative,
                    "bytes": artifact_bytes,
                    "role": role,
                    "sha256": artifact_sha,
                }
            )
    return sorted(values, key=lambda value: (value["bundle_manifest_path"], value["role"]))


def _paper_validator() -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        "taskgate_online_pack_rq5_evidence", PAPER_VALIDATOR
    )
    _require(spec is not None and spec.loader is not None, f"cannot import {PAPER_VALIDATOR}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def validate_pack(pack: pathlib.Path | str = DEFAULT_PACK) -> dict[str, Any]:
    root = pathlib.Path(pack).resolve()
    _require(root.is_dir(), f"pack directory does not exist: {root}")
    manifest, manifest_raw = load_json(root / "pack-manifest.json")
    _exact_fields(
        manifest,
        {
            "schema_version",
            "run_id",
            "rows_per_publication",
            "source_run_relative",
            "retained_file_count",
            "retained_bytes",
            "content_tree_sha256",
            "online_evidence_sha256",
            "files",
            "binary_payload_policy",
            "validation_contract",
        },
        "pack manifest",
    )
    _require(manifest["schema_version"] == PACK_SCHEMA, "pack schema differs")
    _require(manifest["run_id"] == RUN_ID, "run ID differs")
    _require(manifest["rows_per_publication"] == ROWS, "row count differs")
    _require(
        manifest["source_run_relative"]
        == f"evaluation/daily-publication-online/raw/{RUN_ID}",
        "source run path differs",
    )
    _require(
        manifest["validation_contract"]
        == "paper/tkde/rq5_evidence.py:validate_rq5",
        "validation contract differs",
    )

    expected = set(required_paths())
    files = manifest["files"]
    _require(isinstance(files, dict), "manifest files must be an object")
    _require(set(files) == expected, "manifest required file set differs")
    actual_files, actual_directories = _actual_tree(root)
    _require(
        actual_files == expected | {"pack-manifest.json"},
        "pack exact file set differs",
    )
    _require(
        actual_directories == _expected_directories(),
        "pack exact directory set differs",
    )

    retained_bytes = 0
    for relative in sorted(expected):
        descriptor = _exact_fields(
            files[relative], {"bytes", "role", "sha256"}, f"manifest file {relative}"
        )
        body = read_regular(root / relative)
        _require(descriptor["role"] == role_for(relative), f"{relative} role differs")
        _require(descriptor["bytes"] == len(body), f"{relative} byte count differs")
        _require(
            _digest(descriptor["sha256"], f"{relative} SHA-256") == sha256_bytes(body),
            f"{relative} SHA-256 mismatch",
        )
        retained_bytes += len(body)
    _require(manifest["retained_file_count"] == len(expected), "retained file count differs")
    _require(manifest["retained_bytes"] == retained_bytes, "retained byte total differs")
    _require(
        _digest(manifest["content_tree_sha256"], "content tree SHA-256")
        == sha256_bytes(canonical_bytes(files)),
        "content tree SHA-256 mismatch",
    )
    _require(
        _digest(manifest["online_evidence_sha256"], "online evidence SHA-256")
        == files["online-evidence.json"]["sha256"],
        "online evidence manifest binding differs",
    )

    omissions = _expected_omissions(root)
    policy = _exact_fields(
        manifest["binary_payload_policy"],
        {
            "hot_cold_sidecar_bytes_retained",
            "descriptors_retained",
            "omitted_artifact_count",
            "logical_bytes_omitted",
            "artifacts",
            "audit_boundary",
        },
        "binary payload policy",
    )
    _require(policy["hot_cold_sidecar_bytes_retained"] is False, "payload policy differs")
    _require(policy["descriptors_retained"] is True, "descriptor policy differs")
    _require(policy["omitted_artifact_count"] == len(omissions), "omission count differs")
    _require(
        policy["logical_bytes_omitted"] == sum(value["bytes"] for value in omissions),
        "omitted logical byte total differs",
    )
    _require(policy["artifacts"] == omissions, "omitted artifact inventory differs")
    _require(
        policy["audit_boundary"]
        == (
            "A fresh clone re-hashes every retained descriptor and validates its digest chain, "
            "but cannot independently re-hash omitted HOT/COLD/sidecar payload bytes."
        ),
        "omission audit boundary differs",
    )

    validator = _paper_validator()
    try:
        combined = validator.validate_rq5(
            validator.DEFAULT_OFFLINE_PACK, root / "online-evidence.json"
        )
    except validator.EvidenceError as exc:
        raise EvidenceError(f"online evidence pack: RQ5 validation failed: {exc}") from exc
    _require(combined.get("status") == "complete", "combined RQ5 evidence is incomplete")
    _require(
        combined.get("online", {}).get("all_five_conditions_pass") is True,
        "online correctness conditions did not all pass",
    )

    return {
        "schema_version": PACK_SCHEMA,
        "status": "complete",
        "run_id": RUN_ID,
        "rows_per_publication": ROWS,
        "retained_file_count": len(expected),
        "pack_file_count": len(expected) + 1,
        "retained_bytes": retained_bytes,
        "manifest_bytes": len(manifest_raw),
        "pack_bytes": retained_bytes + len(manifest_raw),
        "manifest_sha256": sha256_bytes(manifest_raw),
        "content_tree_sha256": manifest["content_tree_sha256"],
        "omitted_artifact_count": len(omissions),
        "logical_bytes_omitted": policy["logical_bytes_omitted"],
        "transition_count": combined["online"]["transition_count"],
        "all_five_conditions_pass": combined["online"]["all_five_conditions_pass"],
    }


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pack", type=pathlib.Path, default=DEFAULT_PACK)
    parser.add_argument("--json", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    try:
        result = validate_pack(arguments.pack)
    except (OSError, EvidenceError) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    if arguments.json:
        print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
    else:
        print(
            "online evidence pack valid: "
            f"files={result['pack_file_count']} "
            f"bytes={result['pack_bytes']} "
            f"transitions={result['transition_count']} "
            f"manifest_sha256={result['manifest_sha256']}"
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
