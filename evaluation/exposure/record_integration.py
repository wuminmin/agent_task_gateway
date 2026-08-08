#!/usr/bin/env python3
"""Bind RQ3 PostgreSQL tests to their raw go-test JSON evidence."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def relative(path: Path) -> str:
    return path.resolve().relative_to(ROOT).as_posix()


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--raw-output", type=Path, required=True)
    parser.add_argument("--artifact", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--exit-code", type=int, required=True)
    parser.add_argument("--command", required=True)
    parser.add_argument("--go-version", required=True)
    args = parser.parse_args()

    report = json.loads(args.report.read_text(encoding="utf-8"))
    rq3 = report["rq3_anti_arbitrage"]
    manifest = rq3["postgres_integration_manifest"]
    expected = {(item["package"], item["test"]): item["id"] for item in manifest}
    case_ids = {item["id"] for item in manifest}
    if len(expected) != len(manifest) or len(case_ids) != len(manifest) or len(expected) != 5:
        raise SystemExit("RQ3 integration manifest must contain exactly five unique cases")

    destination_args = (args.raw_output, args.artifact, args.output)
    destinations = tuple(destination.resolve() for destination in destination_args)
    if len(set(destinations)) != len(destinations):
        raise SystemExit("RQ3 evidence destinations must be distinct")
    for destination_arg, destination in zip(destination_args, destinations, strict=True):
        relative(destination)
        if not destination.parent.is_dir():
            raise SystemExit(f"RQ3 evidence destination directory is absent: {destination.parent}")
        if destination_arg.is_symlink() or (destination_arg.exists() and not destination_arg.is_file()):
            raise SystemExit(f"RQ3 evidence destination must be a regular file or absent: {destination_arg}")
    if len({destination.parent.stat().st_dev for destination in destinations}) != 1:
        raise SystemExit("RQ3 evidence destinations must share one filesystem")

    raw_log = args.log.read_bytes()
    final_events, package_passed, timestamps = parse_integration_log(raw_log, expected)
    tests = summarize_tests(expected, final_events)
    passed = sum(item["status"] == "pass" for item in tests)
    failed = len(tests) - passed
    expected_packages = {package for package, _ in expected}
    complete = args.exit_code == 0 and package_passed == expected_packages and failed == 0
    artifact_document = {
        "schema_version": 2,
        "status": "complete" if complete else "failed",
        "command": args.command,
        "command_exit_code": args.exit_code,
        "race_enabled": "-race" in args.command.split(),
        "packages": sorted(expected_packages),
        "go_version": args.go_version.strip(),
        "postgres_version": report["rq2_rewrite_invariance"]["postgres_version"],
        "raw_log": relative(args.raw_output),
        "raw_log_sha256": "",
        "started_at": min(timestamps) if timestamps else None,
        "finished_at": max(timestamps) if timestamps else None,
        "executed": len(tests),
        "passed": passed,
        "failed": failed,
        "tests": tests,
    }

    # All canonical invocations share the results directory. Lock its inode so
    # independently successful publishers cannot interleave their replacements.
    publish_lock = os.open(destinations[2].parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        fcntl.flock(publish_lock, fcntl.LOCK_EX)
        stage_and_publish(
            args,
            report,
            artifact_document,
            raw_log,
            expected,
            complete,
            len(tests),
            passed,
            failed,
        )
    finally:
        os.close(publish_lock)


def parse_integration_log(
    raw_log: bytes,
    expected: dict[tuple[str, str], str],
) -> tuple[dict[tuple[str, str], dict], set[str], list[str]]:
    try:
        text = raw_log.decode("utf-8", errors="strict")
    except UnicodeDecodeError as exc:
        raise SystemExit(f"invalid UTF-8 in go-test JSON: {exc}") from exc

    final_events: dict[tuple[str, str], dict] = {}
    package_terminal: dict[str, str] = {}
    timestamps: list[str] = []
    for line_number, raw in enumerate(text.splitlines(), 1):
        try:
            event = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise SystemExit(f"invalid go-test JSON at line {line_number}: {exc}") from exc
        if not isinstance(event, dict):
            raise SystemExit(f"invalid go-test JSON object at line {line_number}")
        package = event.get("Package")
        if isinstance(event.get("Time"), str):
            timestamps.append(event["Time"])
        test = event.get("Test")
        action = event.get("Action")
        if isinstance(package, str) and isinstance(test, str):
            key = (package, test)
            if key in expected and action in {"pass", "fail", "skip"}:
                final_events[key] = event
        if isinstance(package, str) and not test and action in {"pass", "fail", "skip"}:
            package_terminal[package] = action
    package_passed = {package for package, action in package_terminal.items() if action == "pass"}
    return final_events, package_passed, timestamps


def summarize_tests(
    expected: dict[tuple[str, str], str],
    final_events: dict[tuple[str, str], dict],
) -> list[dict]:
    tests = []
    for (package, test), case_id in sorted(expected.items(), key=lambda item: item[1]):
        event = final_events.get((package, test), {})
        tests.append(
            {
                "id": case_id,
                "package": package,
                "test": test,
                "status": event.get("Action", "missing"),
                "elapsed_seconds": event.get("Elapsed"),
            }
        )
    return tests


def stage_and_publish(
    args: argparse.Namespace,
    report: dict,
    artifact_document: dict,
    raw_log: bytes,
    expected: dict[tuple[str, str], str],
    complete: bool,
    executed: int,
    passed: int,
    failed: int,
) -> None:
    with tempfile.TemporaryDirectory(prefix=".record-integration.", dir=args.output.parent) as staging:
        staging_dir = Path(staging)
        staged_raw = staging_dir / "rq3-postgres-go-test.jsonl"
        staged_artifact = staging_dir / "rq3-integration.json"
        staged_output = staging_dir / "results.json"
        staged_raw.write_bytes(raw_log)
        artifact_document["raw_log_sha256"] = digest(staged_raw)
        staged_artifact.write_text(
            json.dumps(artifact_document, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        report["rq3_anti_arbitrage"]["postgres_integration"] = {
            "status": artifact_document["status"],
            "artifact": relative(args.artifact),
            "artifact_sha256": digest(staged_artifact),
            "executed": executed,
            "passed": passed,
            "failed": failed,
        }
        staged_output.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")

        if not complete:
            raise SystemExit(
                "RQ3 PostgreSQL integration evidence is incomplete; canonical evidence was not changed"
            )
        validate_staged_evidence(
            staged_raw,
            staged_artifact,
            staged_output,
            args.raw_output,
            args.artifact,
            expected,
        )

        # results.json is intentionally published last. Each replace is atomic
        # on the shared destination filesystem; this does not claim power-loss
        # atomicity across all three paths.
        os.replace(staged_raw, args.raw_output)
        os.replace(staged_artifact, args.artifact)
        os.replace(staged_output, args.output)


def validate_staged_evidence(
    raw: Path,
    artifact: Path,
    output: Path,
    published_raw: Path,
    published_artifact: Path,
    expected: dict[tuple[str, str], str],
) -> None:
    artifact_document = json.loads(artifact.read_text(encoding="utf-8"))
    output_document = json.loads(output.read_text(encoding="utf-8"))
    integration = output_document["rq3_anti_arbitrage"]["postgres_integration"]
    final_events, package_passed, _ = parse_integration_log(raw.read_bytes(), expected)
    replayed_tests = summarize_tests(expected, final_events)
    expected_packages = {package for package, _ in expected}
    if artifact_document.get("schema_version") != 2 or artifact_document.get("status") != "complete":
        raise SystemExit("staged RQ3 artifact is not complete schema v2 evidence")
    if artifact_document.get("command_exit_code") != 0 or not artifact_document.get("race_enabled"):
        raise SystemExit("staged RQ3 artifact does not bind a successful race-enabled command")
    if artifact_document.get("executed") != 5 or artifact_document.get("passed") != 5 or artifact_document.get("failed") != 0:
        raise SystemExit("staged RQ3 artifact is not complete 5/5/0 evidence")
    if artifact_document.get("packages") != sorted(expected_packages) or package_passed != expected_packages:
        raise SystemExit("staged RQ3 artifact does not carry every package-level pass")
    if artifact_document.get("tests") != replayed_tests or any(item["status"] != "pass" for item in replayed_tests):
        raise SystemExit("staged RQ3 artifact does not match the named raw-log test events")
    if artifact_document.get("raw_log") != relative(published_raw):
        raise SystemExit("staged RQ3 artifact does not reference the canonical raw log")
    if artifact_document.get("raw_log_sha256") != digest(raw):
        raise SystemExit("staged RQ3 artifact raw-log digest is not closed")
    if integration.get("status") != "complete" or integration.get("executed") != 5 or integration.get("passed") != 5 or integration.get("failed") != 0:
        raise SystemExit("staged exposure results are not complete 5/5/0 integration evidence")
    if integration.get("artifact") != relative(published_artifact):
        raise SystemExit("staged exposure results do not reference the canonical RQ3 artifact")
    if integration.get("artifact_sha256") != digest(artifact):
        raise SystemExit("staged exposure results artifact digest is not closed")
    validate_complete_evaluation(output_document)


def validate_complete_evaluation(document: dict) -> None:
    rq1 = document.get("rq1_ground_truth", {})
    rewrite = document.get("rq2_rewrite_invariance", {})
    exposure = document.get("rq2_exposure_invariance", {})
    rq3 = document.get("rq3_anti_arbitrage", {})
    rq4 = document.get("rq4_scaling", {})
    postgres_version = rewrite.get("postgres_version")
    curves = rq4.get("curves")
    requirements = {
        "schema_version=7": document.get("schema_version") == 7,
        "RQ1=21/21": rq1.get("cases") == 21 and rq1.get("passed") == 21,
        "RQ2 PostgreSQL=1024/1024": (
            rewrite.get("generated_attempts") == 1024
            and rewrite.get("unique_normalized_pairs") == 1024
            and rewrite.get("executed_unique_pairs") == 1024
            and rewrite.get("duplicate_attempts") == 0
            and rewrite.get("mismatches") == 0
            and rewrite.get("postgres_major") == 16
            and isinstance(postgres_version, str)
            and postgres_version.startswith("16.14 ")
        ),
        "RQ2 exposure=complete/0": exposure.get("status") == "complete" and exposure.get("mismatches") == 0,
        "RQ3 deterministic=5/5": rq3.get("deterministic_cases") == 5 and rq3.get("deterministic_passed") == 5,
        "RQ4=complete/3": rq4.get("status") == "complete" and isinstance(curves, list) and len(curves) == 3,
    }
    unsatisfied = [name for name, satisfied in requirements.items() if not satisfied]
    if unsatisfied:
        raise SystemExit("staged exposure report is incomplete: " + ", ".join(unsatisfied))


if __name__ == "__main__":
    main()
