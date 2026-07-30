#!/usr/bin/env python3
"""One-time deterministic materializer for the completed RQ5 evidence pack.

This script records already-observed environment values; it does not inspect a
new machine or rerun the campaign.  It refuses to overwrite any derived file.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import tarfile
from typing import Any

import validate


CAPTURED_AT = "2026-07-30T13:29:38.707572569Z"
SUPPLEMENTAL_CAPTURED_AT = "2026-07-30T13:36:02.624404000Z"


def _write_exclusive(path: pathlib.Path, value: dict[str, Any]) -> None:
    encoded = validate.pretty_bytes(value)
    try:
        with path.open("xb") as target:
            target.write(encoded)
    except FileExistsError as exc:
        raise validate.EvidenceError(f"refusing to overwrite sealed file {path}") from exc


def _archive_members(path: pathlib.Path) -> dict[str, dict[str, Any]]:
    values: dict[str, dict[str, Any]] = {}
    with tarfile.open(path, "r:") as archive:
        for member in archive.getmembers():
            stream = archive.extractfile(member)
            if stream is None:
                raise validate.EvidenceError(f"cannot read archive member {member.name}")
            body = stream.read()
            values[member.name] = {"bytes": len(body), "sha256": validate.sha256_bytes(body)}
    return dict(sorted(values.items()))


def _archive_descriptor(pack: pathlib.Path, relative: str) -> dict[str, Any]:
    path = pack / relative
    return {
        "bytes": path.stat().st_size,
        "deterministic_mtime_unix": 0,
        "format": "gnu-tar",
        "numeric_owner": "0:0",
        "path": relative,
        "sha256": validate.file_sha256(path),
    }


def source_manifest(pack: pathlib.Path) -> dict[str, Any]:
    results = validate.load_json(pack / "results.json")
    run_files = results["provenance"]["exact_source"]["files"]
    recorded = _archive_members(pack / "source/run-source.tar")
    if {name: value["sha256"] for name, value in recorded.items()} != run_files:
        raise validate.EvidenceError("run source archive differs from completed results")
    supplemental = _archive_members(pack / "source/post-run-supplemental-source.tar")
    return {
        "schema_version": validate.SOURCE_SCHEMA,
        "run_id": validate.RUN_ID,
        "run_bound_archive": _archive_descriptor(pack, "source/run-source.tar"),
        "run_bound_combined_sha256": results["provenance"]["exact_source"]["combined_sha256"],
        "run_bound_members": recorded,
        "post_run_supplemental_archive": _archive_descriptor(
            pack, "source/post-run-supplemental-source.tar"
        ),
        "post_run_supplemental_captured_at": SUPPLEMENTAL_CAPTURED_AT,
        "post_run_supplemental_is_run_bound": False,
        "post_run_supplemental_members": supplemental,
        "provenance_limitation": (
            "The original results map covered exactly 33 source files. "
            "db/init/00-schema.sql and the two Dockerfile-run *_test.go files were not covered "
            "by that run-bound hash; their bytes were captured only after completion and are supplemental."
        ),
    }


def environment_record(pack: pathlib.Path, source: dict[str, Any]) -> dict[str, Any]:
    results = validate.load_json(pack / "results.json")
    maximum_builder_rss = max(
        int(sample["peak_rss_bytes"]["build"])
        for day in results["offline"]["days"]
        for sample in day["samples"]
    )
    return {
        "schema_version": validate.ENVIRONMENT_SCHEMA,
        "run_id": validate.RUN_ID,
        "captured_at": CAPTURED_AT,
        "capture_timing": "post-run; immediately after campaign completion",
        "bindings": {
            "results_sha256": validate.file_sha256(pack / "results.json"),
            "run_bound_source_archive_sha256": source["run_bound_archive"]["sha256"],
            "run_bound_source_combined_sha256": source["run_bound_combined_sha256"],
            "git_commit_reported_by_results": results["provenance"]["git"]["commit"],
            "git_worktree_dirty_reported_by_results": results["provenance"]["git"]["dirty"],
        },
        "host": {
            "operating_system": "Ubuntu 22.04.5 LTS",
            "kernel": "6.18.33.2-microsoft-standard-WSL2",
            "architecture": "x86_64",
            "cpu_model": "13th Gen Intel(R) Core(TM) i9-13900HX",
            "logical_cpus": 32,
            "memory_total_bytes": 16_625_901_568,
            "swap_total_bytes": 25_769_803_776,
            "filesystem": {
                "mount_source": "/dev/sdd",
                "mount_target": "/",
                "type": "ext4",
                "block_size_bytes": 4096,
                "mount_options": "rw,relatime,discard,errors=remount-ro,data=ordered",
            },
            "virtualization": "Microsoft WSL2 / Docker Desktop",
        },
        "container_runtime": {
            "client_version": "29.1.3",
            "client_git_commit": "f52814d",
            "server_version": "29.1.3",
            "server_git_commit": "fbf3ed2",
            "server_platform": "Docker Desktop 4.55.0 (213807)",
            "compose_version": "v2.40.3-desktop.1",
            "kernel": "6.18.33.2-microsoft-standard-WSL2",
            "architecture": "x86_64",
            "cpus": 32,
            "memory_bytes": 16_625_901_568,
            "cgroup_driver": "cgroupfs",
            "cgroup_version": "2",
            "storage_driver": "overlay2",
            "storage_backing_filesystem": "extfs",
        },
        "images": {
            "phase": {
                "reference": "taskgate-daily-scale-20260730-final3-phase:latest",
                "image_id": "sha256:f8356b61119299ee44a158948aa48bb118f6c5c4856e30c702e00c31c7a5e03e",
                "repo_digest": None,
                "created": "2026-07-30T12:47:44.481859936Z",
                "size_bytes": 102_123_363,
                "rootfs_layers": [
                    "sha256:81f823b9617547261c907396f63f770deaa554748ff739bedfa650e3bb74595a",
                    "sha256:658409cb8c66beb8709b6d201abbeae3f141ee88b8fd44ffe1382e51f39e74fb",
                    "sha256:333599b54b0dedd5225fbcf3296ed8f96add2266f0a0e06e4ead825fbaf16607",
                    "sha256:94308d06f16966f93a65245ae159c76f45a8edac186a6b6a0a0183b871a6e492",
                    "sha256:f8de93d1c9e3497deaf28d6a8adf9ca2426685a2aaf7d6d9a5d07e99e4913675",
                ],
            },
            "postgres": {
                "reference": "postgres:16-bookworm",
                "image_id": "sha256:67d1da22f4037b29cdd93e03d870a4a1c4d079358367d0cbc56459e52cde205e",
                "repo_digest": "postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55",
                "created": "2026-07-14T01:36:25.908336587Z",
                "postgres_version": "16.14-1.pgdg12+1",
            },
            "golang_build_base": {
                "reference": "golang:1.25-bookworm",
                "image_id": "sha256:bd70579a624df39bce056a80ccda1b689b89a6e8393ceaef5878b1a087ac7b1b",
                "repo_digest": "golang@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58",
                "created": "2026-07-14T03:18:20.294866996Z",
                "go_version": "1.25.12",
            },
            "debian_runtime_base": {
                "reference": "debian:bookworm-slim",
                "image_id": "sha256:cae69e86e0b024efa293e7ae0c5760d765422473437056e03d7d941fdf24dd8e",
                "repo_digest": "debian@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818",
                "created": "2026-07-13T00:00:00Z",
            },
            "capture_limitation": (
                "Image IDs and tag resolutions were inspected immediately after completion. "
                "The run retained no container inspect record or BuildKit provenance attestation; "
                "the phase content ID is immutable, while base-image attribution is post-run evidence."
            ),
        },
        "resource_boundary": {
            "phase_container_memory_limit_bytes": validate.PHASE_MEMORY_LIMIT_BYTES,
            "compose_declaration": "mem_limit: 6g",
            "compose_sha256": source["run_bound_members"][
                "evaluation/daily-publication/compose.yaml"
            ]["sha256"],
            "cgroup_memory_max_retained": False,
            "cgroup_memory_peak_retained": False,
            "cgroup_memory_events_retained": False,
            "cgroup_swap_peak_retained": False,
            "builder_peak_rss_max_bytes": maximum_builder_rss,
            "rss_is_cgroup_or_system_peak": False,
            "prior_4_gib_builder_envelope_bytes": validate.PRIOR_BUILDER_ENVELOPE_BYTES,
            "prior_4_gib_builder_envelope_satisfied": False,
            "interpretation": (
                "The 6 GiB value is the run-bound Compose declaration. Actual cgroup peak/events "
                "were not retained; measured VmHWM is only the direct v4-offline process."
            ),
        },
        "cache_policy": {
            "calibration_precedes_measured_builds_for_each_day": True,
            "filesystem_page_cache_dropped": False,
            "postgres_buffers_reset_between_phases": False,
            "strict_verify_immediately_follows_each_build": True,
            "interpretation": "sequential warm operational path; not a cold-cache measurement",
        },
        "measurement_boundary": {
            "phase_report_text": validate.MEASUREMENT_BOUNDARY,
            "rss_scope": validate.RSS_SCOPE,
            "cycle_definition": (
                "sum of three non-contiguous child-process wall times: "
                "build + strict_verify + activation"
            ),
            "timing_clock": "Go monotonic time.Since around direct child Start/Wait",
            "rss_sampling": "1 ms polling of /proc/<direct-child-pid>/status VmHWM and VmRSS",
            "included": ["direct v4-offline child execution"],
            "excluded": [
                "Compose/container startup",
                "campaign orchestration and inter-phase gaps",
                "calibration",
                "PostgreSQL process memory",
                "container page cache and other cgroup memory",
                "host/system peak memory",
            ],
            "full_end_to_end_rollout_elapsed": False,
        },
    }


def transport_omissions(pack: pathlib.Path) -> dict[str, Any]:
    artifacts: list[dict[str, Any]] = []
    for bundle_path in sorted(pack.rglob("*.bundle.json")):
        bundle = validate.load_json(bundle_path)
        relative = bundle_path.relative_to(pack).as_posix()
        for role in ("hot", "cold", "sidecar"):
            descriptor = bundle[role]
            artifacts.append({
                "artifact_path": (pathlib.PurePosixPath(relative).parent / descriptor["name"]).as_posix(),
                "bundle_manifest_path": relative,
                "bytes": descriptor["bytes"],
                "role": role,
                "sha256": descriptor["sha256"],
            })
    artifacts.sort(key=lambda item: (item["bundle_manifest_path"], item["role"]))
    return {
        "schema_version": validate.OMISSION_SCHEMA,
        "run_id": validate.RUN_ID,
        "artifact_count": len(artifacts),
        "logical_bytes_omitted": sum(item["bytes"] for item in artifacts),
        "artifacts": artifacts,
        "audit_boundary": (
            "The compact pack retains bundle and receipt descriptors but not HOT/COLD/sidecar "
            "payload bytes. A fresh clone cannot independently re-hash the omitted payloads."
        ),
        "post_run_spot_check": (
            "One sample-1 HOT/COLD/sidecar set per day was re-hashed after completion and matched, "
            "but that check was not durably recorded by the campaign and is not a pack gate."
        ),
    }


def _role(relative: str) -> str:
    if relative == "results.json":
        return "original-results"
    if relative == "dataset-manifest.json":
        return "dataset-manifest"
    if relative == "environment.json":
        return "post-run-environment-provenance"
    if relative == "source-manifest.json":
        return "source-provenance"
    if relative == "transport-omissions.json":
        return "omitted-binary-inventory"
    if relative == "canonical-offline.json":
        return "recomputed-canonical-summary"
    if relative == "source/run-source.tar":
        return "run-bound-source-archive"
    if relative == "source/post-run-supplemental-source.tar":
        return "post-run-supplemental-source-archive"
    if relative.startswith("candidate-inputs/"):
        return "candidate-input"
    if relative.startswith("approved-inputs/"):
        return "approved-input"
    if relative.endswith(".bundle.json"):
        return "bundle-manifest"
    if "/receipt/" in relative:
        return "verification-receipt"
    if relative.startswith("calibration/"):
        return "calibration-phase-report"
    if relative.startswith("raw/"):
        return "measured-phase-report"
    raise validate.EvidenceError(f"no evidence role for {relative}")


def pack_manifest(pack: pathlib.Path) -> dict[str, Any]:
    manifest_path = pack / "pack-manifest.json"
    files: dict[str, dict[str, Any]] = {}
    for path in sorted(pack.rglob("*")):
        if not path.is_file() or path == manifest_path:
            continue
        relative = path.relative_to(pack).as_posix()
        files[relative] = {
            "bytes": path.stat().st_size,
            "role": _role(relative),
            "sha256": validate.file_sha256(path),
        }
    return {
        "schema_version": validate.SCHEMA,
        "run_id": validate.RUN_ID,
        "sealed_from_completed_results_at": validate.load_json(pack / "results.json")["generated_at"],
        "files": files,
        "combined_sha256": validate.sha256_bytes(validate.canonical_bytes(files)),
        "results_sha256": validate.file_sha256(pack / "results.json"),
        "canonical_offline_sha256": validate.file_sha256(pack / "canonical-offline.json"),
        "binary_payload_policy": {
            "hot_cold_sidecar_bytes_retained": False,
            "descriptors_retained": True,
            "inventory": "transport-omissions.json",
        },
        "integrity_anchor": (
            "This manifest is integrity evidence relative to the Git/release object that contains it; "
            "it is not an external signature or transparency-log entry."
        ),
    }


def seal(pack: pathlib.Path) -> None:
    source = source_manifest(pack)
    _write_exclusive(pack / "source-manifest.json", source)
    _write_exclusive(pack / "environment.json", environment_record(pack, source))
    _write_exclusive(pack / "transport-omissions.json", transport_omissions(pack))
    _write_exclusive(pack / "canonical-offline.json", validate.recompute_canonical(pack))
    _write_exclusive(pack / "pack-manifest.json", pack_manifest(pack))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pack", type=pathlib.Path, default=validate.DEFAULT_PACK)
    args = parser.parse_args()
    try:
        seal(args.pack.resolve())
    except validate.EvidenceError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
