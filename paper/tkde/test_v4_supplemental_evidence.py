#!/usr/bin/env python3
"""Adversarial tests for the V4 supplemental evidence validator.

All synthetic reports live only in memory or a TemporaryDirectory.  They are
validator fixtures, not source-controlled empirical evidence.
"""

from __future__ import annotations

import copy
import gzip
import hashlib
import io
import json
import os
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from paper.tkde import v4_supplemental_evidence as evidence


ROOT = Path(__file__).resolve().parents[2]
HISTORICAL_PROVENANCE_RAW = (ROOT / evidence.HISTORICAL_SOURCE_REL).read_bytes()
HISTORICAL_ARCHIVE_RAW = (ROOT / evidence.HISTORICAL_ARCHIVE_REL).read_bytes()


def _hex(label: str) -> str:
    return hashlib.sha256(label.encode()).hexdigest()


def _json(value: object) -> bytes:
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def _deterministic_gzip(raw: bytes) -> bytes:
    compressed = bytearray(gzip.compress(raw, compresslevel=9, mtime=0))
    compressed[9] = 3
    return bytes(compressed)


def _rewritten_historical_archive(*, replace: dict[str, bytes] | None = None,
                                  remove: set[str] | None = None,
                                  extra: list[tuple[tarfile.TarInfo, bytes | None]] | None = None
                                  ) -> bytes:
    replace, remove, extra = replace or {}, remove or set(), extra or []
    entries: list[tuple[tarfile.TarInfo, bytes | None]] = []
    with tarfile.open(fileobj=io.BytesIO(HISTORICAL_ARCHIVE_RAW), mode="r:gz") as source:
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


def _validate_rebound_historical_archive(archive_raw: bytes) -> dict[str, object]:
    provenance = evidence._decode_json(
        HISTORICAL_PROVENANCE_RAW, "test historical provenance")
    archive_sha = hashlib.sha256(archive_raw).hexdigest()
    provenance["archive_sha256"] = archive_sha
    with mock.patch.object(evidence, "EXPECTED_HISTORICAL_ARCHIVE_SHA256", archive_sha):
        return evidence._historical_source_snapshot(_json(provenance), archive_raw)


def _historical_member(name: str, raw: bytes = b"x", *,
                       member_type: bytes = tarfile.REGTYPE,
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


def _bitmap(cardinality: int, label: str, *, containers: int = 1,
            portable_bytes: int = 8, minimum: int = 0, maximum: int = 10) -> dict[str, object]:
    digest = _hex(label)
    result: dict[str, object] = {
        "cardinality": cardinality,
        "container_count": containers if cardinality else 0,
        "portable_bitmap_bytes": portable_bytes if cardinality else 0,
        "digest": digest,
        "portable_round_trip_verified": True,
        "round_trip_digest": digest,
        "has_ordinals": cardinality > 0,
    }
    if cardinality:
        result["minimum_ordinal"] = minimum
        result["maximum_ordinal"] = maximum
    return result


def _latency(runs: int) -> dict[str, object]:
    samples = [1.0 + index / 1000 for index in range(runs)]
    return {"samples_ms": samples, "summary": evidence._distribution(samples)}


def _distribution_report() -> tuple[dict[str, object], str]:
    runs = 50
    cells: list[dict[str, object]] = []
    cell_digests: list[str] = []
    for shape in ("dense", "clustered", "random_sparse"):
        physical = evidence.EXPECTED_DISTRIBUTION_EFFECTS[shape]
        effect = _bitmap(
            1_035_000, shape + "-effect",
            containers=int(physical["container_count"]),
            portable_bytes=int(physical["portable_bitmap_bytes"]),
            minimum=int(physical["minimum_ordinal"]),
            maximum=int(physical["maximum_ordinal"]),
        )
        effect["digest"] = physical["digest"]
        effect["round_trip_digest"] = physical["digest"]
        observation = evidence._distribution_observation_digest(str(effect["digest"]), 1_035_000)
        for overlap in (0, 50, 90, 100):
            overlap_count = 1_035_000 * overlap // 100
            before = (copy.deepcopy(effect) if overlap == 100 else
                      _bitmap(overlap_count, f"{shape}-{overlap}-before", maximum=int(physical["maximum_ordinal"])))
            novel_count = 1_035_000 - overlap_count
            novel = _bitmap(novel_count, f"{shape}-{overlap}-novel",
                             maximum=int(physical["maximum_ordinal"]))
            replay = _bitmap(0, "empty")
            cell: dict[str, object] = {
                "distribution": shape,
                "target_overlap_percent": overlap,
                "observed_overlap_percent": float(overlap),
                "effect": copy.deepcopy(effect),
                "ledger_before": before,
                "novel_delta": novel,
                "ledger_after": copy.deepcopy(effect),
                "replay_delta": replay,
                "observation_sha256": observation,
                "replay_observation_sha256": observation,
                "replay_matched": True,
                "andnot_or_latency_ms": _latency(runs),
                "replay_digest_lookup_latency_ms": _latency(runs),
                "replay_lookups_per_run": 4096,
                "construction_and_encode_ms": 2.0,
                "memory": {
                    "start_heap_alloc_bytes": 100,
                    "end_heap_alloc_bytes": 150,
                    "peak_heap_alloc_bytes": 200,
                    "peak_heap_inuse_bytes": 300,
                    "peak_heap_sys_bytes": 400,
                    "peak_heap_delta_bytes": 100,
                    "total_alloc_delta_bytes": 500,
                },
            }
            cell_digest = evidence._distribution_cell_digest(cell)
            cell["deterministic_cell_sha256"] = cell_digest
            cell_digests.append(cell_digest)
            cells.append(cell)
    matrix = evidence._framed_zero_digest(b"taskgate-v4-bitmap-distribution-v2\0", cell_digests)
    report: dict[str, object] = {
        "schema_version": 2,
        "status": "complete_measured_kernel",
        "generator_version": "taskgate-v4-bitmap-distribution-v2",
        "scope": "ordinal BitmapSet kernel only; excludes Gateway, PostgreSQL, networking, encryption, CAS, and result persistence",
        "started_at": "2026-07-30T00:00:00Z",
        "finished_at": "2026-07-30T00:00:01Z",
        "configuration": {
            "cardinality": 1_035_000, "runs": runs, "cluster_count": 128,
            "random_seed": 0x6D2B79F5, "replay_lookups_per_run": 4096,
            "max_peak_heap_bytes": 512 << 20,
        },
        "runtime": {"go_version": "go1.24.5", "goos": "linux", "goarch": "amd64", "cpus": 8},
        "metric_semantics": {
            "andnot_or_latency_ms": "measured", "replay_digest_lookup_latency_ms": "measured",
            "portable_bitmap_bytes": "measured", "portable_round_trip_verified": "full",
            "peak_heap_delta_bytes": "measured", "total_alloc_delta_bytes": "measured",
            "construction_and_encode_ms": "measured",
        },
        "cells": cells,
        "matrix_sha256": matrix,
        "acceptance_eligible": True,
    }
    return report, matrix


def _concurrency_config() -> dict[str, object]:
    cases: list[dict[str, object]] = []
    identities = ((1, "release"), (4, "influence"), (8, "outcome"), (16, "release"))
    for level, dimension in identities:
        case_id = f"same-root-{dimension}-c{level}"
        cases.append({
            "id": case_id, "concurrency": level, "boundary_dimension": dimension,
            "root_task_id": case_id + "-root", "prefix_task_id": case_id + "-prefix",
            "contender_task_ids": [f"{case_id}-contender-{index}" for index in range(level)],
            "overflow_task_id": case_id + "-overflow",
            "prefix_plan": {"plan": case_id + "-prefix"},
            "contender_plan": {"plan": case_id + "-contender"},
            "overflow_plan": {"plan": case_id + "-overflow"},
            "before_used": {"release": 1, "influence": 2, "outcome": 1},
            "at_budget": {"release": 2, "influence": 3, "outcome": 2},
        })
    return {
        "schema_version": 1,
        "gateway": {
            "url": "http://127.0.0.1:8082",
            "contender_urls": ["http://127.0.0.1:8082", "http://127.0.0.1:8083"],
            "token_env": "TASKBOUND_ALICE_TOKEN",
        },
        "control_dsn_env": "V4_CONCURRENCY_CONTROL_DSN",
        "request_timeout_ms": 30_000, "lock_wait_timeout_ms": 15_000,
        "provision": {
            "oa_url": "http://127.0.0.1:8092", "alice_password_env": "OA_ALICE_PASSWORD",
            "bob_password_env": "OA_BOB_PASSWORD", "data_products": ["expense_detail"],
            "columns": {"expense_detail": ["receipt_no", "amount", "city"]},
            "scopes": {"department": ["sales"]},
        },
        "cases": cases,
    }


def _head(epoch: int, used: dict[str, int], limits: dict[str, int], label: str,
          *, commitments: bool) -> dict[str, object]:
    result: dict[str, object] = {"epoch": epoch, "limits": copy.deepcopy(limits), "used": copy.deepcopy(used)}
    if commitments:
        for dimension in ("release", "influence", "outcome"):
            result[dimension + "_set_sha256"] = _hex(label + "-" + dimension)
    return result


def _concurrency_report(config: dict[str, object], config_raw: bytes) -> dict[str, object]:
    cells: list[dict[str, object]] = []
    gates: list[dict[str, object]] = [{
        "id": "source_and_config_binding", "requirement": "binding", "status": "pass",
    }]
    for case in config["cases"]:  # type: ignore[index]
        level = case["concurrency"]
        case_id = case["id"]
        before_used = case["before_used"]
        budget = case["at_budget"]
        before = _head(1, before_used, budget, case_id + "-before", commitments=True)
        at = _head(2, budget, budget, case_id + "-at", commitments=True)
        observation, result = _hex(case_id + "-observation"), _hex(case_id + "-result")
        content = {"containers": 3, "sets": 7, "dynamic_facts": 2, "observations": 2}
        cells.append({
            "case_id": case_id, "concurrency": level,
            "boundary_dimension": case["boundary_dimension"],
            "root_task_sha256": evidence._sha(case["root_task_id"].encode()),
            "family_task_sha256": sorted(evidence._sha(one.encode()) for one in [
                case["prefix_task_id"], case["overflow_task_id"], *case["contender_task_ids"]]),
            "status": "measured",
            "initial": _head(0, {"release": 0, "influence": 0, "outcome": 0}, budget,
                             case_id + "-initial", commitments=False),
            "before_boundary": before, "at_boundary": at,
            "after_rejected_overflow": copy.deepcopy(at),
            "prefix": {
                "status": "measured", "latency_ms": 1.0,
                "observation_sha256": _hex(case_id + "-prefix-observation"),
                "actual": copy.deepcopy(before_used), "charged": copy.deepcopy(before_used),
                "root_epoch": 1, "result_sha256": _hex(case_id + "-prefix-result"),
            },
            "contention": {
                "status": "measured", "root_lock_waiters_observed": level,
                "successful_requests": level,
                "failed_requests": 0, "charged_winners": 1,
                "zero_novelty_settlements": level - 1,
                "total_charged": {"release": 1, "influence": 1, "outcome": 1},
                "root_epochs": [2] * level, "observation_sha256": [observation] * level,
                "result_sha256": [result] * level, "client_latency_ms": [1.0] * level,
            },
            "overflow": {
                "status": "rejected", "expected_error_code": "EXPOSURE_BUDGET_EXHAUSTED",
                "observed_error_code": "EXPOSURE_BUDGET_EXHAUSTED", "latency_ms": 1.0,
                "query_status": "FAILED", "exposure_reservation_status": "RELEASED",
                "encrypted_results": 0, "encrypted_result_chunks": 0, "materializations": 0,
                "query_observations": 0, "root_observations": 0,
                "terminal_success_audits": 0, "terminal_failure_audits": 1, "receipts": 1,
                "content_before": copy.deepcopy(content), "content_after": copy.deepcopy(content),
            },
            "checks": {
                "shared_root_family": True, "fresh_root": True, "b_minus_one_committed": True,
                "b_committed": True, "three_dimensional_atomic": True,
                "root_lock_queue_observed": True, "overflow_rejected": True,
                "failure_left_no_partial_commit": True,
            },
        })
        prefix = "case_" + evidence._safe_id(case_id) + "_"
        for suffix in ("shared_root", "fresh_root", "b_minus_one", "b",
                       "three_dimensional_atomicity", "root_lock_queue", "b_plus_one", "failure_atomicity"):
            gates.append({"id": prefix + suffix, "requirement": suffix, "status": "pass"})
    for gate_id in ("concurrency_widths", "boundary_dimensions", "all_root_lock_queues"):
        gates.append({"id": gate_id, "requirement": gate_id, "status": "pass"})
    return {
        "schema_version": 2, "status": "complete_measured_campaign", "acceptance": "pass",
        "started_at": "2026-07-30T00:00:00Z", "finished_at": "2026-07-30T00:00:01Z",
        "configuration": {
            "gateway_url": config["gateway"]["url"],
            "contender_gateway_urls": config["gateway"]["contender_urls"],
            "contender_gateway_count": 2, "per_gateway_control_pool": 10,
            "request_timeout_ms": config["request_timeout_ms"],
            "lock_wait_timeout_ms": config["lock_wait_timeout_ms"], "case_count": 4,
            "concurrency_levels": [1, 4, 8, 16],
            "boundary_dimensions": ["influence", "outcome", "release"],
        },
        "provenance": {
            "config_sha256": evidence._sha(config_raw),
            "source_sha256": evidence._concurrency_source_scope(ROOT)[0],
        },
        "metric_notes": {
            "client_latency_ms": "measured", "root_lock_waiters": "measured",
            "inference_boundary": "does not infer CAS reads, conflicts, or retries",
            "failure_audit": "checked",
            "gateway_replicas": "two identical replicas",
        },
        "cells": cells, "gates": gates,
    }


def _oracle_report(results: dict[str, object], results_sha: str) -> dict[str, object]:
    expected = evidence._maximum_point_identity(results)
    repository_sha, paths = evidence._oracle_repository_source_scope(ROOT)
    facts = {
        "expected_release": 12, "actual_release": 12, "matched_release": 12,
        "expected_influence": 1_035_000, "actual_influence": 1_035_000,
        "matched_influence": 1_035_000, "expected_outcome": 1, "actual_outcome": 1,
        "matched_outcome": 1, "fact_hash_matches": evidence.TOTAL_MAX_POINT,
        "canonical_payload_matches": evidence.TOTAL_MAX_POINT,
        "total_compared": evidence.TOTAL_MAX_POINT, "hash_mismatches": 0,
        "canonical_payload_mismatches": 0, "missing_facts": 0, "extra_facts": 0,
        "influence_chunk_sha256": [_hex(f"chunk-{index}") for index in range(16)],
    }
    return {
        "schema_version": "taskgate-v4-million-fact-oracle-v1",
        "oracle_id": "taskgate-independent-external-merge-oracle-v1", "status": "pass",
        "started_at": "2026-07-30T00:00:00Z", "finished_at": "2026-07-30T00:00:01Z",
        "provenance": {
            "results_sha256": results_sha,
            "oracle_package_sha256": evidence._oracle_package_digest(ROOT),
            "repository_source_scope_sha256": repository_sha,
            "repository_source_scope_files": len(paths), "executable_sha256": _hex("executable"),
            "cold_artifacts": [{
                "publication_name": name, "dictionary_sha256": _hex(name + "-dictionary"),
                "manifest_sha256": _hex(name + "-manifest"),
                "artifact_sha256": _hex(name + "-artifact"), "bytes": 1,
            } for name in ("scale-orders-v4-narrow-1", "scale-lineitem-v4-narrow-1")],
        },
        "independence_boundary": {
            "expected_source": "independent row-wise reconstruction from frozen reporting.scale_orders and reporting.scale_lineitem",
            "actual_source": "committed Control-PG containers plus independently streamed COLD FactIDs",
            "algorithm": "bounded external merge sort by full FactHash; exact canonical-payload and witness-multiplicity comparison",
            "independence_scope": "derivation-independent; shares the versioned canonical FactID specification and encoder with TaskGate",
            "evidence_validation": "strict duplicate-key/trailing JSON rejection plus full-file SHA-256 binding of source-controlled V4 results",
            "v4_bitmap_derivation_hot_path_calls": 0,
        },
        "observation": {**expected, "recomputed_sha256": expected["sha256"],
                        "normal_form_sha256": _hex("normal-form")},
        "fact_checks": facts,
        "witness_checks": {
            "derived_facts": 12, "matched_commitments": 12, "commitment_mismatches": 0,
            "expected_witness_items": 1_035_000, "expected_total_multiplicity": 1_800_000,
            "commitment_set_sha256": _hex("commitments"),
            "multiplicity_stream_sha256": _hex("multiplicity"),
        },
        "resources": {
            "sort_memory_limit_bytes": 1 << 20, "theoretical_buffer_bound_bytes": 14 << 20,
            "spool_bytes": 1, "sort_runs": 2,
            "sort_run_sha256": [_hex("sort-0"), _hex("sort-1")],
            "maximum_resident_records": 1, "peak_rss_bytes": 1,
            "business_rows": 225_000, "cold_facts_scanned": 2_600_000,
        },
        "gates": [{"id": gate, "requirement": gate, "status": "pass", "evidence": {}}
                  for gate in sorted(evidence.EXPECTED_ORACLE_GATES)],
    }


class StrictJSONTests(unittest.TestCase):
    def test_duplicate_nonfinite_trailing_and_unknown_are_rejected(self) -> None:
        malformed = (b'{"a":1,"a":2}', b'{"a":NaN}', b'{"a":Infinity}', b'{"a":1} trailing')
        for raw in malformed:
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                evidence._decode_json(raw, "fixture")
        with self.assertRaises(ValueError):
            evidence._fields({"known": 1, "unknown": 2}, {"known"}, "fixture")
        with self.assertRaises(ValueError):
            evidence._integer(True, "fixture")

    def test_inline_go_slice_is_parsed_without_losing_entries(self) -> None:
        source = 'var repositorySourceFiles = []string{"go.mod","go.sum"}\n'
        self.assertEqual(evidence._parse_go_string_slice(source, "repositorySourceFiles"),
                         ["go.mod", "go.sum"])
        actual = (ROOT / evidence.ORACLE_SOURCE_REL).read_text(encoding="utf-8")
        self.assertEqual(evidence._parse_go_string_slice(actual, "repositorySourceFiles"),
                         ["go.mod", "go.sum"])


class HistoricalSourceSnapshotTests(unittest.TestCase):
    def test_canonical_archive_reproduces_all_reported_source_bindings(self) -> None:
        snapshot = evidence._historical_source_snapshot(
            HISTORICAL_PROVENANCE_RAW, HISTORICAL_ARCHIVE_RAW)
        self.assertEqual(snapshot["source_scope_sha256"],
                         evidence.EXPECTED_HISTORICAL_SCOPE_SHA256)
        self.assertEqual(snapshot["source_scope_files"], 138)
        self.assertEqual(snapshot["concurrency_source_sha256"],
                         evidence.EXPECTED_HISTORICAL_CONCURRENCY_SHA256)
        self.assertEqual(snapshot["concurrency_source_files"], 113)
        self.assertEqual(snapshot["oracle_repository_sha256"],
                         evidence.EXPECTED_HISTORICAL_ORACLE_REPOSITORY_SHA256)
        self.assertEqual(snapshot["oracle_repository_files"], 52)
        self.assertEqual(snapshot["oracle_package_sha256"],
                         evidence.EXPECTED_HISTORICAL_ORACLE_PACKAGE_SHA256)
        self.assertEqual(snapshot["archive_member_count"], 157)

    def test_outer_archive_digest_tamper_is_rejected(self) -> None:
        changed = bytearray(HISTORICAL_ARCHIVE_RAW)
        changed[len(changed) // 2] ^= 1
        with self.assertRaisesRegex(ValueError, "archive digest is stale"):
            evidence._historical_source_snapshot(
                HISTORICAL_PROVENANCE_RAW, bytes(changed))

    def test_inner_source_tamper_is_rejected_after_outer_digest_rebind(self) -> None:
        name = "internal/gateway/query.go"
        with tarfile.open(fileobj=io.BytesIO(HISTORICAL_ARCHIVE_RAW), mode="r:gz") as archive:
            original = archive.extractfile(name).read()
        changed = _rewritten_historical_archive(
            replace={name: original + b"\n// tampered\n"})
        with self.assertRaisesRegex(ValueError, "bindings are not reproducible"):
            _validate_rebound_historical_archive(changed)

    def test_missing_extra_unsafe_link_and_duplicate_members_are_rejected(self) -> None:
        cases = [
            _rewritten_historical_archive(remove={"internal/gateway/query.go"}),
            _rewritten_historical_archive(extra=[
                _historical_member("internal/gateway/not-in-scope.go", b"package gateway\n")]),
            _rewritten_historical_archive(extra=[_historical_member("../escape.go")]),
            _rewritten_historical_archive(extra=[
                _historical_member("unsafe-link.go", member_type=tarfile.SYMTYPE,
                                   linkname="internal/gateway/query.go")]),
            _rewritten_historical_archive(extra=[
                _historical_member("internal/gateway/query.go", b"package gateway\n")]),
        ]
        for archive_raw in cases:
            with self.subTest(sha256=hashlib.sha256(archive_raw).hexdigest()), \
                    self.assertRaises(ValueError):
                _validate_rebound_historical_archive(archive_raw)


class ManifestTests(unittest.TestCase):
    def _directory(self, parent: Path) -> tuple[Path, dict[str, object]]:
        directory = parent / "candidate-evidence"
        directory.mkdir()
        raw_by_name = {name: (b"readme\n" if name == "README.md" else b"{}\n")
                       for name in evidence.LOCAL_ARTIFACTS}
        for name, raw in raw_by_name.items():
            (directory / name).write_bytes(raw)
        scope_sha, scope_files, _ = evidence._generalized_source_scope(ROOT)
        artifacts = {
            f"{evidence.EVIDENCE_REL.as_posix()}/{name}": evidence._sha(raw)
            for name, raw in raw_by_name.items()
        }
        base_raw = (ROOT / evidence.V4_RESULTS_REL).read_bytes()
        artifacts[evidence.V4_RESULTS_REL.as_posix()] = evidence._sha(base_raw)
        manifest: dict[str, object] = {
            "schema_version": 1, "status": "complete_supplemental_campaign",
            "source_scope": {"sha256": scope_sha, "files": scope_files,
                             "algorithm": evidence.MANIFEST_SOURCE_ALGORITHM},
            "artifacts": artifacts,
        }
        (directory / "manifest.json").write_bytes(_json(manifest))
        return directory, manifest

    def test_configurable_directory_and_exact_manifest_pass(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory, _ = self._directory(Path(temporary))
            _, retained = evidence._load_manifest(ROOT, directory)
            self.assertEqual(set(retained), evidence.EXPECTED_ARTIFACTS)

    def test_missing_and_unknown_files_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory, _ = self._directory(Path(temporary))
            (directory / "distribution.json").unlink()
            with self.assertRaisesRegex(ValueError, "directory set differs"):
                evidence._load_manifest(ROOT, directory)
        with tempfile.TemporaryDirectory() as temporary:
            directory, _ = self._directory(Path(temporary))
            (directory / "unexpected.json").write_bytes(b"{}\n")
            with self.assertRaisesRegex(ValueError, "directory set differs"):
                evidence._load_manifest(ROOT, directory)

    def test_tampered_artifact_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory, _ = self._directory(Path(temporary))
            (directory / "distribution.json").write_bytes(b'{"tampered":true}\n')
            with self.assertRaisesRegex(ValueError, "artifact SHA-256 differs"):
                evidence._load_manifest(ROOT, directory)

    def test_stale_source_scope_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory, manifest = self._directory(Path(temporary))
            manifest["source_scope"]["sha256"] = "0" * 64  # type: ignore[index]
            (directory / "manifest.json").write_bytes(_json(manifest))
            with self.assertRaisesRegex(ValueError, "source scope is stale"):
                evidence._load_manifest(ROOT, directory)

    def test_symlink_artifact_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory, _ = self._directory(Path(temporary))
            (directory / "environment.json").unlink()
            os.symlink("README.md", directory / "environment.json")
            with self.assertRaises(ValueError):
                evidence._load_manifest(ROOT, directory)


class SemanticReportTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.results_raw = (ROOT / evidence.V4_RESULTS_REL).read_bytes()
        cls.results = evidence._decode_json(cls.results_raw, "base results")

    def test_distribution_report_passes_then_detects_tamper(self) -> None:
        report, matrix = _distribution_report()
        with mock.patch.object(evidence, "EXPECTED_DISTRIBUTION_MATRIX_SHA256", matrix):
            evidence._validate_distribution_report(copy.deepcopy(report))
            report["cells"][0]["effect"]["cardinality"] -= 1  # type: ignore[index,operator]
            with self.assertRaises(ValueError):
                evidence._validate_distribution_report(report)

    def test_concurrency_report_passes_then_detects_tamper(self) -> None:
        config = _concurrency_config()
        config_raw = _json(config)
        validated = evidence._validate_concurrency_config(copy.deepcopy(config))
        report = _concurrency_report(config, config_raw)
        evidence._validate_concurrency_report(copy.deepcopy(report), validated, config_raw, ROOT)
        report["cells"][1]["contention"]["root_lock_waiters_observed"] = 0  # type: ignore[index]
        with self.assertRaises(ValueError):
            evidence._validate_concurrency_report(report, validated, config_raw, ROOT)
        source_tamper = _concurrency_report(config, config_raw)
        source_tamper["provenance"]["source_sha256"] = "0" * 64  # type: ignore[index]
        with self.assertRaisesRegex(ValueError, "source binding is stale"):
            evidence._validate_concurrency_report(
                source_tamper, validated, config_raw, ROOT)

    def test_oracle_report_passes_then_detects_tamper(self) -> None:
        report = _oracle_report(self.results, evidence._sha(self.results_raw))
        evidence._validate_oracle_report(copy.deepcopy(report), self.results,
                                         evidence._sha(self.results_raw), ROOT)
        report["fact_checks"]["canonical_payload_mismatches"] = 1  # type: ignore[index]
        with self.assertRaises(ValueError):
            evidence._validate_oracle_report(report, self.results,
                                             evidence._sha(self.results_raw), ROOT)
        source_tamper = _oracle_report(self.results, evidence._sha(self.results_raw))
        source_tamper["provenance"]["repository_source_scope_sha256"] = "0" * 64  # type: ignore[index]
        with self.assertRaisesRegex(ValueError, "source/executable binding is stale"):
            evidence._validate_oracle_report(
                source_tamper, self.results, evidence._sha(self.results_raw), ROOT)

    def test_environment_rejects_secret_text(self) -> None:
        result_sha = evidence._sha(self.results_raw)
        value = {
            "schema_version": 2, "captured_at": "2026-07-30T00:00:00Z",
            "host": {"kernel": "Linux", "architecture": "x86_64", "cpu_model": "test",
                     "logical_cpus": 8, "memory_bytes": 8 << 30},
            "software": {"go_version": "go1.24.5", "postgres_version": "PostgreSQL 16.3",
                         "images": {name: "sha256:" + _hex(name) for name in
                                    ("go_build", "postgres", "concurrency_gateway",
                                     "concurrency_gateway_peer", "concurrency_oa")},
                         "concurrency_gateway_binary_sha256": _hex("gateway-binary")},
            "base_v4": {"campaign_id": "taskgate-v4-full-20260730t070232z",
                        "results_sha256": result_sha},
            "datasets": {
                "business": {"identity_sha256": _hex("business"), "name": "business",
                             "snapshot_id": "exposure-scale-2026-v4-narrow-1",
                             "scale_orders_rows": 50_000, "scale_lineitem_rows": 250_000,
                             "frozen": True},
                "control": {"identity_sha256": _hex("control"), "name": "control",
                            "catalog_sha256": _hex("catalog"),
                            "dictionary_set_sha256": _hex("dictionary"),
                            "exposure_profile": "taskgate-exposure-v4"},
                "concurrency": {
                    "project": "taskgate-v4-concurrency-final",
                    "snapshot_id": "travel-demo-2026-v1", "expense_detail_rows": 10,
                    "catalog_sha256": evidence._sha(
                        (ROOT / "evaluation/v4-concurrency/catalog.yaml").read_bytes()),
                    "gateway_replicas": 2, "per_gateway_control_pool": 10, "frozen": True,
                },
            },
        }
        evidence._validate_environment(copy.deepcopy(value), result_sha)
        value["datasets"]["business"]["name"] = "postgres://user:password@example/db"  # type: ignore[index]
        with self.assertRaisesRegex(ValueError, "credential-like"):
            evidence._validate_environment(value, result_sha)


class SourceControlledEvidenceTests(unittest.TestCase):
    def test_canonical_evidence_directory_when_present(self) -> None:
        directory = ROOT / evidence.EVIDENCE_REL
        if not directory.exists():
            self.skipTest("supplemental campaign has not yet been archived")
        validated = evidence.validate_v4_supplemental_evidence(ROOT)
        self.assertEqual(validated["stats"]["distribution_cells"], 12)
        self.assertEqual(validated["stats"]["concurrency_cells"], 4)
        self.assertEqual(validated["stats"]["oracle_total_compared"], evidence.TOTAL_MAX_POINT)
        self.assertEqual(validated["current_source_relation"]["status"], "diverged")
        self.assertFalse(validated["current_source_relation"]["matches_historical"])


if __name__ == "__main__":
    unittest.main()
