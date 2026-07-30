#!/usr/bin/env python3
"""Deterministically seal the completed 345k RQ5 online evidence run.

Only descriptor-sized evidence is copied.  HOT, COLD, and sidecar payloads
remain represented by the names, byte counts, and SHA-256 values committed in
the retained bundle manifests.  The output directory must not already exist.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import pathlib
import shutil
import stat
import sys
import tempfile
from types import ModuleType
from typing import Any


HERE = pathlib.Path(__file__).resolve().parent
REPOSITORY_ROOT = HERE.parents[2]
RUN_ID = "scale-20260730-final"
ROWS = 345_000
DAYS = ("day0", "day1", "day2", "day3")
PACK_SCHEMA = "taskgate-daily-publication-online-pack-v1"
DEFAULT_SOURCE = REPOSITORY_ROOT / "evaluation/daily-publication-online/raw" / RUN_ID
DEFAULT_OUTPUT = HERE / RUN_ID
PAPER_VALIDATOR = REPOSITORY_ROOT / "paper/tkde/rq5_evidence.py"
MAX_DESCRIPTOR_BYTES = 16 << 20


class SealError(ValueError):
    """Raised when source evidence cannot be sealed without ambiguity."""


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
            raise SealError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def _reject_nonfinite(value: str) -> Any:
    raise SealError(f"non-finite JSON number {value!r}")


def read_regular(path: pathlib.Path) -> bytes:
    try:
        before = os.lstat(path)
    except OSError as exc:
        raise SealError(f"cannot stat {path}: {exc}") from exc
    if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
        raise SealError(f"{path} must be a regular non-symlink file")
    if before.st_size <= 0 or before.st_size > MAX_DESCRIPTOR_BYTES:
        raise SealError(f"{path} has invalid descriptor size {before.st_size}")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "rb") as source:
            opened = os.fstat(source.fileno())
            body = source.read(MAX_DESCRIPTOR_BYTES + 1)
            after = os.fstat(source.fileno())
        after_path = os.lstat(path)
    except OSError as exc:
        raise SealError(f"cannot read {path}: {exc}") from exc
    identity = lambda value: (value.st_dev, value.st_ino, value.st_size, value.st_mtime_ns)
    if not (
        identity(before) == identity(opened) == identity(after) == identity(after_path)
        and len(body) == before.st_size
    ):
        raise SealError(f"{path} changed while being read")
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
        raise SealError(f"invalid JSON in {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise SealError(f"{path} must contain one JSON object")
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
    raise SealError(f"unclassified retained path {relative}")


def _paper_validator() -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        "taskgate_online_pack_rq5_evidence", PAPER_VALIDATOR
    )
    if spec is None or spec.loader is None:
        raise SealError(f"cannot import {PAPER_VALIDATOR}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def validate_online_source(source: pathlib.Path) -> None:
    validator = _paper_validator()
    try:
        result = validator.validate_rq5(
            validator.DEFAULT_OFFLINE_PACK, source / "online-evidence.json"
        )
    except validator.EvidenceError as exc:
        raise SealError(f"source online evidence failed RQ5 validation: {exc}") from exc
    if result.get("status") != "complete":
        raise SealError("source online evidence is not complete")


def _omissions(pack: pathlib.Path) -> list[dict[str, Any]]:
    values: list[dict[str, Any]] = []
    for day in DAYS:
        name = publication_name(day)
        relative = f"artifacts/{name}/{name}.bundle.json"
        bundle, _ = load_json(pack / relative)
        if bundle.get("publication_name") != name:
            raise SealError(f"{relative} publication identity differs")
        for role, suffix in (
            ("hot", ".hot.tgord"),
            ("cold", ".cold.tgord"),
            ("sidecar", ".sidecar.ndjson"),
        ):
            descriptor = bundle.get(role)
            if not isinstance(descriptor, dict):
                raise SealError(f"{relative} lacks {role} descriptor")
            artifact_name = descriptor.get("name")
            artifact_bytes = descriptor.get("bytes")
            artifact_sha = descriptor.get("sha256")
            if artifact_name != name + suffix:
                raise SealError(f"{relative} has unexpected {role} name")
            if (
                not isinstance(artifact_bytes, int)
                or isinstance(artifact_bytes, bool)
                or artifact_bytes <= 0
            ):
                raise SealError(f"{relative} has invalid {role} byte count")
            if (
                not isinstance(artifact_sha, str)
                or len(artifact_sha) != 64
                or any(character not in "0123456789abcdef" for character in artifact_sha)
            ):
                raise SealError(f"{relative} has invalid {role} SHA-256")
            values.append(
                {
                    "artifact_path": f"artifacts/{name}/{artifact_name}",
                    "bundle_manifest_path": relative,
                    "bytes": artifact_bytes,
                    "role": role,
                    "sha256": artifact_sha,
                }
            )
    return sorted(values, key=lambda value: (value["bundle_manifest_path"], value["role"]))


def pack_manifest(pack: pathlib.Path) -> dict[str, Any]:
    files: dict[str, dict[str, Any]] = {}
    for relative in required_paths():
        body = read_regular(pack / relative)
        files[relative] = {
            "bytes": len(body),
            "role": role_for(relative),
            "sha256": sha256_bytes(body),
        }
    omissions = _omissions(pack)
    return {
        "schema_version": PACK_SCHEMA,
        "run_id": RUN_ID,
        "rows_per_publication": ROWS,
        "source_run_relative": f"evaluation/daily-publication-online/raw/{RUN_ID}",
        "retained_file_count": len(files),
        "retained_bytes": sum(value["bytes"] for value in files.values()),
        "content_tree_sha256": sha256_bytes(canonical_bytes(files)),
        "online_evidence_sha256": files["online-evidence.json"]["sha256"],
        "files": files,
        "binary_payload_policy": {
            "hot_cold_sidecar_bytes_retained": False,
            "descriptors_retained": True,
            "omitted_artifact_count": len(omissions),
            "logical_bytes_omitted": sum(value["bytes"] for value in omissions),
            "artifacts": omissions,
            "audit_boundary": (
                "A fresh clone re-hashes every retained descriptor and validates its digest chain, "
                "but cannot independently re-hash omitted HOT/COLD/sidecar payload bytes."
            ),
        },
        "validation_contract": "paper/tkde/rq5_evidence.py:validate_rq5",
    }


def _copy_required(source: pathlib.Path, target: pathlib.Path) -> None:
    for relative in required_paths():
        body = read_regular(source / relative)
        destination = target / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        try:
            with destination.open("xb") as output:
                output.write(body)
        except FileExistsError as exc:
            raise SealError(f"refusing to overwrite {destination}") from exc
        destination.chmod(0o644)


def _pack_validator() -> ModuleType:
    path = HERE / "validate.py"
    spec = importlib.util.spec_from_file_location("taskgate_online_pack_validator", path)
    if spec is None or spec.loader is None:
        raise SealError(f"cannot import {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def seal(source: pathlib.Path, output: pathlib.Path) -> dict[str, Any]:
    source = source.resolve()
    output = output.resolve()
    if not source.is_dir():
        raise SealError(f"source run directory does not exist: {source}")
    if output.exists():
        raise SealError(f"refusing to overwrite evidence pack: {output}")
    validate_online_source(source)
    output.parent.mkdir(parents=True, exist_ok=True)
    staging = pathlib.Path(
        tempfile.mkdtemp(prefix=f".{output.name}.seal-", dir=output.parent)
    )
    try:
        _copy_required(source, staging)
        manifest = pack_manifest(staging)
        with (staging / "pack-manifest.json").open("xb") as destination:
            destination.write(pretty_bytes(manifest))
        (staging / "pack-manifest.json").chmod(0o644)
        validator = _pack_validator()
        validator.validate_pack(staging)
        staging.rename(output)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    return manifest


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=pathlib.Path, default=DEFAULT_SOURCE)
    parser.add_argument("--output", type=pathlib.Path, default=DEFAULT_OUTPUT)
    return parser


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    try:
        manifest = seal(arguments.source, arguments.output)
    except (OSError, SealError) as exc:
        print(f"daily-publication-online seal: {exc}", file=sys.stderr)
        return 1
    manifest_path = arguments.output.resolve() / "pack-manifest.json"
    print(
        "sealed online evidence: "
        f"files={manifest['retained_file_count']} "
        f"bytes={manifest['retained_bytes']} "
        f"manifest_sha256={sha256_bytes(read_regular(manifest_path))}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
