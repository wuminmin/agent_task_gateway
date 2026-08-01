#!/usr/bin/env python3
"""Fail-closed tests for the retained V4 historical source snapshot."""

from __future__ import annotations

import copy
import gzip
import hashlib
import io
import json
import os
import shutil
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from paper.tkde import v4_evidence as evidence


ROOT = Path(__file__).resolve().parents[2]
PROVENANCE_RAW = (ROOT / evidence.HISTORICAL_SOURCE_REL).read_bytes()
ARCHIVE_RAW = (ROOT / evidence.HISTORICAL_ARCHIVE_REL).read_bytes()
ENVIRONMENT_RAW = (ROOT / evidence.EVIDENCE_REL / "environment.json").read_bytes()


def _encoded(value: dict[str, object]) -> bytes:
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def _deterministic_gzip(raw: bytes) -> bytes:
    compressed = bytearray(gzip.compress(raw, compresslevel=9, mtime=0))
    compressed[9] = 3  # GNU gzip's Unix OS byte.
    return bytes(compressed)


def _validate_with_rebound_archive(archive_raw: bytes) -> dict[str, object]:
    provenance = evidence._decode_json(PROVENANCE_RAW, "test provenance")
    archive_sha = hashlib.sha256(archive_raw).hexdigest()
    provenance["archive_sha256"] = archive_sha
    with mock.patch.object(evidence, "EXPECTED_HISTORICAL_ARCHIVE_SHA256", archive_sha):
        return evidence._historical_source_snapshot(
            _encoded(provenance), archive_raw, evidence.EXPECTED_HISTORICAL_SOURCE_SHA256)


def _rewritten_archive(*, replace: dict[str, bytes] | None = None,
                       remove: set[str] | None = None,
                       extra: list[tuple[tarfile.TarInfo, bytes | None]] | None = None) -> bytes:
    replace, remove, extra = replace or {}, remove or set(), extra or []
    entries: list[tuple[tarfile.TarInfo, bytes | None]] = []
    with tarfile.open(fileobj=io.BytesIO(ARCHIVE_RAW), mode="r:gz") as source:
        for member in source:
            if member.name in remove:
                continue
            raw = source.extractfile(member).read() if member.isfile() else None
            cloned = copy.copy(member)
            if member.name in replace:
                raw = replace[member.name]
                cloned.size = len(raw)
            entries.append((cloned, raw))
    output = io.BytesIO()
    with tarfile.open(
        fileobj=output, mode="w", format=tarfile.PAX_FORMAT,
        pax_headers={"comment": evidence.EXPECTED_HISTORICAL_COMMIT},
    ) as target:
        for member, raw in [*entries, *extra]:
            target.addfile(member, io.BytesIO(raw) if raw is not None else None)
    return _deterministic_gzip(output.getvalue())


def _member(name: str, raw: bytes = b"x", *, member_type: bytes = tarfile.REGTYPE,
            linkname: str = "") -> tuple[tarfile.TarInfo, bytes | None]:
    member = tarfile.TarInfo(name)
    member.type = member_type
    member.uid = member.gid = 0
    member.uname = member.gname = "root"
    member.mtime = evidence.EXPECTED_HISTORICAL_MTIME
    member.mode = 0o755 if member_type == tarfile.DIRTYPE else 0o644
    member.linkname = linkname
    if member_type == tarfile.REGTYPE:
        member.size = len(raw)
        return member, raw
    member.size = 0
    return member, None


def _small_archive(entries: list[tuple[tarfile.TarInfo, bytes | None]]) -> bytes:
    output = io.BytesIO()
    with tarfile.open(
        fileobj=output, mode="w", format=tarfile.PAX_FORMAT,
        pax_headers={"comment": evidence.EXPECTED_HISTORICAL_COMMIT},
    ) as archive:
        for member, raw in entries:
            archive.addfile(member, io.BytesIO(raw) if raw is not None else None)
    return _deterministic_gzip(output.getvalue())


class HistoricalSourceSnapshotTests(unittest.TestCase):
    def test_canonical_archive_reproduces_measured_source(self) -> None:
        validated = evidence._historical_source_snapshot(
            PROVENANCE_RAW, ARCHIVE_RAW, evidence.EXPECTED_HISTORICAL_SOURCE_SHA256)
        self.assertEqual(validated["source_sha256"], evidence.EXPECTED_HISTORICAL_SOURCE_SHA256)
        self.assertEqual(validated["source_paths_sha256"], evidence.EXPECTED_HISTORICAL_PATHS_SHA256)
        self.assertEqual(validated["source_file_count"], 187)
        self.assertEqual(validated["archive_member_count"], 223)
        self.assertEqual(
            validated["environment_input_sha256"]["compose.yaml"],
            "cc76d61a8425367b26487e6a4630a83df16cae234e379e864a4d5ad0622522a9",
        )

    def test_environment_inputs_are_bound_to_historical_archive(self) -> None:
        validated = evidence._historical_source_snapshot(
            PROVENANCE_RAW, ARCHIVE_RAW, evidence.EXPECTED_HISTORICAL_SOURCE_SHA256)
        environment = evidence._decode_json(ENVIRONMENT_RAW, "test environment")
        evidence._validate_historical_environment_inputs(
            environment, validated["environment_input_sha256"])

        current_compose_sha = hashlib.sha256((ROOT / "compose.yaml").read_bytes()).hexdigest()
        self.assertNotEqual(
            current_compose_sha,
            validated["environment_input_sha256"]["compose.yaml"],
        )
        changed = copy.deepcopy(environment)
        changed["software"]["orchestration_sha256"]["compose"] = current_compose_sha
        with self.assertRaisesRegex(ValueError, "compose.*historical source snapshot"):
            evidence._validate_historical_environment_inputs(
                changed, validated["environment_input_sha256"])

    def test_wrong_commit_is_rejected(self) -> None:
        provenance = evidence._decode_json(PROVENANCE_RAW, "test provenance")
        provenance["git_commit"] = "0" * 40
        with self.assertRaisesRegex(ValueError, "identity/commit"):
            evidence._historical_source_snapshot(
                _encoded(provenance), ARCHIVE_RAW, evidence.EXPECTED_HISTORICAL_SOURCE_SHA256)

    def test_tampered_source_bytes_are_rejected_after_safe_unpack(self) -> None:
        name = "internal/gateway/query.go"
        with tarfile.open(fileobj=io.BytesIO(ARCHIVE_RAW), mode="r:gz") as archive:
            original = archive.extractfile(name).read()
        tampered = _rewritten_archive(replace={name: original + b"\n// tampered\n"})
        with self.assertRaisesRegex(ValueError, "path/content digest"):
            _validate_with_rebound_archive(tampered)

    def test_missing_and_extra_source_members_are_rejected(self) -> None:
        missing = _rewritten_archive(remove={"internal/gateway/query.go"})
        with self.assertRaises(ValueError):
            _validate_with_rebound_archive(missing)
        extra_member = _member("internal/gateway/not-in-historical-scope.go", b"package gateway\n")
        extra = _rewritten_archive(extra=[extra_member])
        with self.assertRaises(ValueError):
            _validate_with_rebound_archive(extra)

    def test_path_traversal_symlink_and_duplicate_members_are_rejected(self) -> None:
        unsafe_archives = [
            _small_archive([_member("../escape.go")]),
            _small_archive([_member("link.go", member_type=tarfile.SYMTYPE, linkname="target")]),
            _small_archive([_member("same.go"), _member("same.go")]),
        ]
        for archive_raw in unsafe_archives:
            with self.subTest(sha256=hashlib.sha256(archive_raw).hexdigest()), \
                    self.assertRaises(ValueError):
                _validate_with_rebound_archive(archive_raw)

    def test_compressed_archive_bound_is_enforced_before_unpack(self) -> None:
        oversized = b"x" * ((4 << 20) + 1)
        provenance = evidence._decode_json(PROVENANCE_RAW, "test provenance")
        sha = hashlib.sha256(oversized).hexdigest()
        provenance["archive_sha256"] = sha
        with mock.patch.object(evidence, "EXPECTED_HISTORICAL_ARCHIVE_SHA256", sha), \
                self.assertRaisesRegex(ValueError, "compressed bound"):
            evidence._historical_source_snapshot(
                _encoded(provenance), oversized, evidence.EXPECTED_HISTORICAL_SOURCE_SHA256)


class V4EvidenceIntegrationTests(unittest.TestCase):
    def test_full_campaign_validates_against_historical_not_current_source(self) -> None:
        validated = evidence.validate_v4_evidence(ROOT)
        self.assertEqual(validated["stats"]["sample_count"], 560)
        self.assertEqual(validated["historical_source"]["git_commit"],
                         evidence.EXPECTED_HISTORICAL_COMMIT)
        relation = validated["current_source_relation"]
        self.assertEqual(relation["status"], "diverged")
        self.assertFalse(relation["matches_historical"])
        self.assertNotEqual(relation["current_source_sha256"],
                            relation["historical_source_sha256"])

    def test_manifest_detects_archive_tamper(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            candidate = Path(temporary) / "evidence"
            shutil.copytree(ROOT / evidence.EVIDENCE_REL, candidate)
            archive = candidate / evidence.HISTORICAL_ARCHIVE_REL.name
            raw = bytearray(archive.read_bytes())
            raw[len(raw) // 2] ^= 1
            archive.write_bytes(raw)
            with self.assertRaisesRegex(ValueError, "artifact digest differs"):
                evidence.validate_v4_evidence(ROOT, candidate)

    def test_symlinked_source_provenance_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            candidate = Path(temporary) / "evidence"
            shutil.copytree(ROOT / evidence.EVIDENCE_REL, candidate)
            provenance = candidate / evidence.HISTORICAL_SOURCE_REL.name
            provenance.unlink()
            os.symlink("README.md", provenance)
            with self.assertRaises(ValueError):
                evidence.validate_v4_evidence(ROOT, candidate)


if __name__ == "__main__":
    unittest.main()
